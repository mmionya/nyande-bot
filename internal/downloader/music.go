package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/internal/music"
)

var ErrMusicTooLong = errors.New("music must be a non-live track no longer than 30 minutes")
var ErrMusicTooLarge = errors.New("music exceeds the upload size limit")

func (d *Downloader) musicArgs() []string {
	args := []string{"--ignore-config", "--no-warnings", "--no-progress", "--no-playlist", "--socket-timeout", "10", "--retries", "1", "--extractor-retries", "1", "--no-cache-dir"}
	if d.cfg.YTDownloadCookies != "" {
		args = append(args, "--cookies", d.cfg.YTDownloadCookies)
	}
	if d.cfg.YTDownloadBrowser != "" {
		args = append(args, "--cookies-from-browser", d.cfg.YTDownloadBrowser)
	}
	return args
}

func (d *Downloader) musicExecutable() string {
	if d.cfg.YTDownloadPath != "" {
		return d.cfg.YTDownloadPath
	}
	return "yt-dlp"
}

type musicMetadataOutput struct{ buffer bytes.Buffer }

func (b *musicMetadataOutput) Write(p []byte) (int, error) {
	if len(p) > (4<<20)-b.buffer.Len() {
		return 0, errors.New("music metadata exceeds 4 MiB")
	}
	return b.buffer.Write(p)
}

func (d *Downloader) SearchMusic(ctx context.Context, query string, limit int) ([]music.Track, error) {
	identifier, err := music.Identifier(query)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	release, err := music.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, d.musicExecutable(), append(d.musicArgs(), args...)...)
		configureYTDLPProcess(cmd)
		cmd.WaitDelay = time.Second
		var output musicMetadataOutput
		cmd.Stdout, cmd.Stderr = &output, io.Discard
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
				_ = cmd.Cancel()
			}
			return nil, fmt.Errorf("music search: %w", err)
		}
		return output.buffer.Bytes(), nil
	}
	return music.Search(ctx, identifier, limit, run, d.client)
}

// DownloadMusic produces one bounded MP3; temporary downloads and conversion
// share the extractor gate with Discord so small servers do not run them in parallel.
func (d *Downloader) DownloadMusic(ctx context.Context, track music.Track) (Media, error) {
	page, err := music.Identifier(track.URL)
	if err != nil || !strings.HasPrefix(page, "https://") {
		return Media{}, errors.New("invalid music track URL")
	}
	if track.Live || track.Duration > 30*time.Minute {
		return Media{}, ErrMusicTooLong
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	release, err := music.Acquire(ctx)
	if err != nil {
		return Media{}, err
	}
	defer release()
	limit := d.cfg.MaxFileSize
	if limit <= 0 || limit > 50*1024*1024 {
		limit = 50 * 1024 * 1024
	}
	ffmpeg := d.cfg.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	ffmpeg, err = exec.LookPath(ffmpeg)
	if err != nil {
		return Media{}, fmt.Errorf("music converter: %w", err)
	}
	ffmpeg, err = filepath.Abs(ffmpeg)
	if err != nil {
		return Media{}, err
	}
	dir, err := os.MkdirTemp("", "nyande-music-*")
	if err != nil {
		return Media{}, err
	}
	defer os.RemoveAll(dir)
	args := append(d.musicArgs(), "--format", "bestaudio/best", "--extract-audio", "--audio-format", "mp3", "--audio-quality", "128K",
		"--postprocessor-args", "ExtractAudio+ffmpeg_o:-threads 1", "--match-filters", "!is_live & duration <=? 1800",
		"--max-filesize", strconv.FormatInt(limit, 10), "--output", filepath.Join(dir, "track.%(ext)s"))
	args = append(args, "--ffmpeg-location", ffmpeg)
	args = append(args, "--", page)
	output, err := runYTDLP(ctx, d.musicExecutable(), args, dir, 2*limit+1024*1024)
	if errors.Is(err, errYTDLPMediaTooLarge) || ytdlpSizeLimitOutput(string(output)) {
		return Media{}, ErrMusicTooLarge
	}
	if err != nil {
		// Do not include signed stream URLs or cookie diagnostics in logs/replies.
		return Media{}, fmt.Errorf("music download: %w", err)
	}
	path := filepath.Join(dir, "track.mp3")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return Media{}, errors.New("extractor did not produce an MP3 track")
	}
	if info.Size() > limit {
		return Media{}, ErrMusicTooLarge
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Media{}, err
	}
	return Media{Kind: "audio", Name: "track.mp3", MIME: "audio/mpeg", Data: data}, nil
}
