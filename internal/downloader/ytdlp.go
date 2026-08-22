package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (d *Downloader) downloadYTDLP(ctx context.Context, value *url.URL) (Result, error) {
	return d.downloadYTDLPWithOptions(ctx, value, false)
}

func (d *Downloader) downloadYTDLPWithOptions(ctx context.Context, value *url.URL, allowPlaylist bool) (Result, error) {
	formats := ytdlpMediaFormats(value)
	var lastErr error
	var sizeErr error
	for index, format := range formats {
		result, err := d.downloadYTDLPAttempt(ctx, value, allowPlaylist, format)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if errors.Is(err, errYTDLPMediaTooLarge) {
			sizeErr = err
		}
		if index == len(formats)-1 || !IsYouTube(value) || ctx.Err() != nil || !youtubeFormatFallbackError(err) {
			break
		}
	}
	if sizeErr != nil && errors.Is(lastErr, errYTDLPNoMedia) {
		return Result{}, sizeErr
	}
	return Result{}, lastErr
}

func (d *Downloader) downloadYTDLPAttempt(ctx context.Context, value *url.URL, allowPlaylist bool, format string) (Result, error) {
	isYouTube := IsYouTube(value)
	directory, err := os.MkdirTemp("", "nyande-ytdlp-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(directory)

	output := filepath.Join(directory, "%(id)s.%(ext)s")
	arguments := []string{
		"--ignore-config", "--no-progress", "--newline",
		"--max-filesize", strconv.FormatInt(d.cfg.MaxFileSize, 10),
		"--write-info-json",
		"--output", output,
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
	command := exec.CommandContext(ctx, d.cfg.YTDownloadPath, arguments...)
	command.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	outputData, err := command.CombinedOutput()
	if err != nil {
		detail := tail(string(outputData), 3000)
		if ytdlpSizeLimitOutput(detail) {
			return Result{}, fmt.Errorf("%w: %s", errYTDLPMediaTooLarge, detail)
		}
		return Result{}, fmt.Errorf("yt-dlp failed: %w: %s", err, detail)
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
		if info.Size() > d.cfg.MaxFileSize {
			oversizedMedia = true
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return Result{}, err
		}
		items = append(items, Media{Kind: kind, Name: entry.Name(), MIME: mimeForName(entry.Name()), Data: data})
	}
	if len(items) == 0 {
		if oversizedMedia || ytdlpSizeLimitOutput(string(outputData)) {
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
		"bestvideo[height<=" + limit + "]+bestaudio/best[height<=" + limit + "]/best"
}

func youtubeFormatFallbackError(err error) bool {
	if errors.Is(err, errYTDLPMediaTooLarge) || errors.Is(err, errYTDLPNoMedia) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "requested format is not available")
}

func ytdlpSizeLimitOutput(value string) bool {
	text := strings.ToLower(value)
	return strings.Contains(text, "max-filesize") ||
		strings.Contains(text, "larger than max") ||
		strings.Contains(text, "exceeds the maximum")
}

var (
	errYTDLPNoMedia       = errors.New("yt-dlp produced no supported media")
	errYTDLPMediaTooLarge = errors.New("yt-dlp media is too large")
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
