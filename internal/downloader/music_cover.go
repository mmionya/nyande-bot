package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mmionya/nyande-bot/internal/music"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const musicCoverDownloadLimit = 4 * 1024 * 1024
const musicThumbnailLimit = 200 * 1024

// Cover failure is optional: never discard a successfully downloaded song.
// Prefer full extraction metadata to the often small/missing flat-search image.
func (d *Downloader) musicCover(ctx context.Context, directory, searchArtwork string) ([]byte, []byte) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var metadata music.Metadata
	infoPath := filepath.Join(directory, "track.info.json")
	if stat, err := os.Lstat(infoPath); err == nil && stat.Mode().IsRegular() && stat.Size() <= musicCoverDownloadLimit {
		if data, err := os.ReadFile(infoPath); err == nil {
			if err := json.Unmarshal(data, &metadata); err != nil {
				metadata = music.Metadata{}
			}
		}
	}
	candidates := []string{metadata.Thumbnail}
	for i := len(metadata.Thumbnails) - 1; i >= 0 && len(candidates) < 3; i-- {
		candidates = append(candidates, metadata.Thumbnails[i].URL)
	}
	candidates = append(candidates, searchArtwork)
	seen := make(map[string]bool)
	// Use the existing public-address and redirect checks with a smaller byte cap.
	images := &Downloader{cfg: d.cfg, client: d.client}
	images.cfg.MaxFileSize = musicCoverDownloadLimit
	images.cfg.VideoCompressionEnabled = false
	for _, raw := range candidates {
		if ctx.Err() != nil {
			break
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		parsed, err := url.Parse(raw)
		if err != nil || len(raw) > 2048 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			continue
		}
		media, err := images.fetchMedia(ctx, raw, "photo")
		if err != nil {
			continue
		}
		cover, thumbnail, err := normalizeMusicCover(media.Data)
		if err == nil {
			return cover, thumbnail
		}
	}
	return nil, nil
}

func normalizeMusicCover(data []byte) ([]byte, []byte, error) {
	if len(data) > musicCoverDownloadLimit {
		return nil, nil, errors.New("cover exceeds byte limit")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	// Check dimensions before decoding to bound memory on small bot hosts.
	if config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 16*1024*1024 {
		return nil, nil, errors.New("cover exceeds pixel limit")
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	cover, err := musicCoverJPEG(source, 1024, 1024*1024)
	if err != nil {
		return nil, nil, err
	}
	thumbnail, err := musicCoverJPEG(source, 320, musicThumbnailLimit-1)
	if err != nil {
		return nil, nil, err
	}
	return cover, thumbnail, nil
}

func musicCoverJPEG(source image.Image, side, byteLimit int) ([]byte, error) {
	width, height := source.Bounds().Dx(), source.Bounds().Dy()
	if width > side || height > side {
		if width >= height {
			height = max(1, height*side/width)
			width = side
		} else {
			width = max(1, width*side/height)
			height = side
		}
	}
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(resized, resized.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(resized, resized.Bounds(), source, source.Bounds(), draw.Over, nil)
	for _, quality := range []int{85, 70, 55} {
		var output bytes.Buffer
		if err := jpeg.Encode(&output, resized, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if output.Len() <= byteLimit {
			return output.Bytes(), nil
		}
	}
	return nil, errors.New("JPEG cover exceeds byte limit")
}

// Copy compressed audio frames unchanged and attach JPEG artwork as an ID3 cover.
// Keep the original file if remuxing fails or would exceed the upload limit.
func embedMusicCover(ctx context.Context, ffmpeg, directory, audio string, cover []byte, limit int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	coverPath := filepath.Join(directory, "cover.jpg")
	if err := os.WriteFile(coverPath, cover, 0600); err != nil {
		return "", err
	}
	output := filepath.Join(directory, "covered.mp3")
	command := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1",
		"-i", audio, "-i", coverPath, "-map", "0:a:0", "-map", "1:v:0", "-map_metadata", "0", "-c", "copy",
		"-id3v2_version", "3", "-disposition:v:0", "attached_pic", "-metadata:s:v:0", "title=Album cover",
		"-metadata:s:v:0", "comment=Cover (front)", "-y", output)
	configureYTDLPProcess(command)
	command.WaitDelay = time.Second
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
			_ = command.Cancel()
		}
		return "", err
	}
	stat, err := os.Lstat(output)
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 || stat.Size() > limit {
		return "", errors.New("covered audio exceeds upload limit or is empty")
	}
	return output, nil
}
