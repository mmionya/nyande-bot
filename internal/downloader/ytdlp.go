package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/resources"
)

func (d *Downloader) downloadYTDLP(ctx context.Context, value *url.URL) (Result, error) {
	return d.downloadYTDLPWithOptions(ctx, value, false)
}

func (d *Downloader) downloadYTDLPWithOptions(ctx context.Context, value *url.URL, allowPlaylist bool) (Result, error) {
	formats := ytdlpMediaFormats(value)
	var lastErr error
	var sizeErr error
	var compressionFormat string
	for index, format := range formats {
		result, err := d.downloadYTDLPAttempt(ctx, value, allowPlaylist, format, d.cfg.VideoCompressionEnabled && !IsYouTube(value))
		if err == nil {
			return result, nil
		}
		lastErr = err
		if errors.Is(err, errYTDLPMediaTooLarge) {
			sizeErr = err
			compressionFormat = format
		}
		if index == len(formats)-1 || !IsYouTube(value) || ctx.Err() != nil || !youtubeFormatFallbackError(err) {
			break
		}
	}
	// YouTube gets every smaller download first. Only a size failure permits
	// one larger-source attempt using the smallest format known to be available.
	if d.cfg.VideoCompressionEnabled && IsYouTube(value) && sizeErr != nil && ctx.Err() == nil &&
		(errors.Is(lastErr, errYTDLPMediaTooLarge) || errors.Is(lastErr, errYTDLPNoMedia) ||
			strings.Contains(strings.ToLower(lastErr.Error()), "requested format is not available")) {
		return d.downloadYTDLPAttempt(ctx, value, allowPlaylist, compressionFormat, true)
	}
	if sizeErr != nil && (errors.Is(lastErr, errYTDLPNoMedia) || strings.Contains(strings.ToLower(lastErr.Error()), "requested format is not available")) {
		return Result{}, sizeErr
	}
	return Result{}, lastErr
}

func (d *Downloader) downloadYTDLPAttempt(ctx context.Context, value *url.URL, allowPlaylist bool, format string, allowCompression bool) (Result, error) {
	isYouTube := IsYouTube(value)
	directory, err := os.MkdirTemp("", "nyande-ytdlp-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(directory)

	output := filepath.Join(directory, "%(id)s.%(ext)s")
	if IsInstagram(value) {
		output = filepath.Join(directory, "%(playlist_index|0)05d-%(id)s.%(ext)s")
	}
	downloadLimit := d.cfg.MaxFileSize
	if allowCompression {
		downloadLimit = d.videoSourceLimit()
	}
	arguments := []string{
		"--ignore-config", "--no-progress", "--newline",
		"--max-filesize", strconv.FormatInt(downloadLimit, 10),
		"--write-info-json",
		"--output", output,
	}
	if IsInstagram(value) {
		pluginDir := filepath.Join(directory, "plugins")
		plugin := filepath.Join(pluginDir, "nyande", "yt_dlp_plugins", "extractor", "instagram_photos.py")
		if err := os.MkdirAll(filepath.Dir(plugin), 0700); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(plugin, instagramPhotosPlugin, 0600); err != nil {
			return Result{}, err
		}
		arguments = append(arguments, "--plugin-dirs", pluginDir)
	}
	arguments = append(arguments, ytdlpMediaOptionsForFormat(value, format)...)
	if allowPlaylist {
		arguments = append(arguments, "--yes-playlist", "--playlist-end", strconv.Itoa(d.cfg.MaxMediaItems))
	} else {
		arguments = append(arguments, "--no-playlist")
	}
	if d.cfg.YTDownloadCookies != "" {
		arguments = append(arguments, "--cookies", d.cfg.YTDownloadCookies)
	}
	if d.cfg.YTDownloadBrowser != "" {
		arguments = append(arguments, "--cookies-from-browser", d.cfg.YTDownloadBrowser)
	}
	arguments = append(arguments, value.String())
	// Allow room for separate streams plus the merged file, and metadata.
	diskLimit := 2*downloadLimit + 1024*1024
	if !d.cfg.VideoCompressionEnabled {
		diskLimit = 2*downloadLimit*int64(max(1, d.cfg.MaxMediaItems)) + 1024*1024
	}
	outputData, err := runYTDLP(ctx, d.cfg.YTDownloadPath, arguments, directory, diskLimit)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		detail := tail(string(outputData), 3000)
		if errors.Is(err, errYTDLPMediaTooLarge) || ytdlpSizeLimitOutput(detail) {
			if allowCompression {
				return Result{}, compressionRefusal("downloader.error.video_source_too_large", downloadLimit/(1024*1024))
			}
			return Result{}, fmt.Errorf("%w: %s", errYTDLPMediaTooLarge, detail)
		}
		return Result{}, fmt.Errorf(resources.Get("downloader.error.yt_dlp_failed"), err, detail)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return Result{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	items := make([]Media, 0)
	caption := ""
	oversizedMedia := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fullPath := filepath.Join(directory, entry.Name())
		if strings.HasSuffix(entry.Name(), ".info.json") {
			if caption == "" {
				caption = ytdlpCaption(fullPath, isYouTube)
			}
			continue
		}
		kind := kindFromExtension(entry.Name())
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".heic", ".heif", ".avif":
			kind = "photo"
		}
		if kind == "" {
			continue
		}
		if isYouTube && kind == "video" && !strings.EqualFold(filepath.Ext(entry.Name()), ".mp4") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Size() > downloadLimit || (info.Size() > d.cfg.MaxFileSize && (kind != "video" || !allowCompression)) {
			if kind == "video" && allowCompression {
				return Result{}, compressionRefusal("downloader.error.video_source_too_large", downloadLimit/(1024*1024))
			}
			oversizedMedia = true
			continue
		}
		if kind == "video" && allowCompression {
			item, err := d.videoMediaFromFile(ctx, fullPath, entry.Name(), mimeForName(entry.Name()))
			if err != nil {
				return Result{}, err
			}
			items = append(items, item)
			if len(items) >= d.cfg.MaxMediaItems {
				break
			}
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return Result{}, err
		}
		name, contentType := entry.Name(), mimeForName(entry.Name())
		if kind == "photo" {
			// Instagram can serve JPEG bytes from a signed URL ending in .heic.
			// Use the actual format without modifying the signed download URL.
			contentType = http.DetectContentType(data)
			extension := map[string]string{
				"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp",
			}[contentType]
			if extension == "" {
				return Result{}, fmt.Errorf(resources.Get("downloader.error.yt_dlp_downloaded_an_unsupported_photo_format"), contentType)
			}
			name = strings.TrimSuffix(name, filepath.Ext(name)) + extension
		}
		items = append(items, Media{Kind: kind, Name: name, MIME: contentType, Data: data})
	}
	if len(items) == 0 {
		if oversizedMedia || ytdlpSizeLimitOutput(string(outputData)) {
			if allowCompression && !oversizedMedia {
				return Result{}, compressionRefusal("downloader.error.video_source_too_large", downloadLimit/(1024*1024))
			}
			return Result{}, errYTDLPMediaTooLarge
		}
		return Result{}, errYTDLPNoMedia
	}
	source := "ytdlp"
	if isYouTube {
		source = "youtube"
	} else if IsReddit(value) {
		source = "reddit"
	} else if IsInstagram(value) {
		source = "instagram"
	} else if IsXiaohongshu(value) {
		source = "xiaohongshu"
	} else if IsPinterest(value) {
		source = "pinterest"
	}
	return Result{Items: items, Caption: caption, Source: source}, nil
}

func ytdlpMediaOptions(value *url.URL) []string {
	return ytdlpMediaOptionsForFormat(value, ytdlpMediaFormats(value)[0])
}

func ytdlpMediaOptionsForFormat(value *url.URL, format string) []string {
	options := []string{"--merge-output-format", "mp4"}
	if IsYouTube(value) {
		options = append(options, "--js-runtimes", "deno", "--recode-video", "mp4")
	}
	if IsInstagram(value) {
		// Bound stalled requests and retries before trying another source.
		// The socket timeout does not cap the duration of an active download.
		options = append(options, "--socket-timeout", "15", "--extractor-retries", "1",
			"--retries", "2", "--fragment-retries", "2")
	}
	return append(options, "--format", format)
}

func ytdlpMediaFormats(value *url.URL) []string {
	if !IsYouTube(value) {
		return []string{"bestvideo[height<=1080]+bestaudio/best[height<=1080]/best"}
	}
	heights := []int{1080, 720, 480, 360, 240, 144}
	formats := make([]string, 0, len(heights))
	for _, height := range heights {
		formats = append(formats, youtubeMediaFormat(height))
	}
	return formats
}

func youtubeMediaFormat(height int) string {
	limit := strconv.Itoa(height)
	return "bestvideo[ext=mp4][vcodec^=av01][height<=" + limit + "]+bestaudio[ext=m4a]/" +
		"bestvideo[ext=mp4][vcodec^=hev1][height<=" + limit + "]+bestaudio[ext=m4a]/" +
		"bestvideo[ext=mp4][vcodec^=hvc1][height<=" + limit + "]+bestaudio[ext=m4a]/" +
		"bestvideo[ext=mp4][vcodec^=avc1][height<=" + limit + "]+bestaudio[ext=m4a]/" +
		"best[ext=mp4][vcodec^=avc1][height<=" + limit + "]/" +
		"bestvideo[height<=" + limit + "]+bestaudio/best[height<=" + limit + "]"
}

func youtubeFormatFallbackError(err error) bool {
	// Lower height selectors are subsets of the first: unavailable formats
	// cannot become available by lowering the ceiling.
	return errors.Is(err, errYTDLPMediaTooLarge)
}

func ytdlpSizeLimitOutput(value string) bool {
	text := strings.ToLower(value)
	return strings.Contains(text, "max-filesize") ||
		strings.Contains(text, "larger than max") ||
		strings.Contains(text, "exceeds the maximum")
}

var (
	errYTDLPNoMedia       = errors.New(resources.Get("downloader.error.yt_dlp_produced_no_supported_media"))
	errYTDLPMediaTooLarge = errors.New(resources.Get("downloader.error.yt_dlp_media_is_too_large"))
)

func ytdlpCaption(filename string, titleOnly bool) string {
	data, err := os.ReadFile(filename)
	if err != nil {
		return ""
	}
	var info map[string]any
	if json.Unmarshal(data, &info) != nil {
		return ""
	}
	return ytdlpCaptionFromInfo(info, titleOnly)
}

func ytdlpCaptionFromInfo(info map[string]any, titleOnly bool) string {
	title, _ := info["title"].(string)
	title = strings.TrimSpace(title)
	if titleOnly {
		return title
	}
	description, _ := info["description"].(string)
	uploader := firstMapString(info, "uploader", "channel", "creator")
	parts := make([]string, 0, 3)
	if title != "" {
		parts = append(parts, title)
	}
	if strings.TrimSpace(description) != "" && strings.TrimSpace(description) != strings.TrimSpace(title) {
		parts = append(parts, strings.TrimSpace(description))
	}
	if strings.TrimSpace(uploader) != "" {
		parts = append(parts, strings.TrimSpace(uploader))
	}
	return strings.Join(parts, "\n\n")
}

func mimeForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	default:
		return "application/octet-stream"
	}
}

func tail(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	return value[len(value)-maximum:]
}
