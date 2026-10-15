package downloader

import (
	"context"
	"io"
	"os"
	"sync"
	"time"
)

type compressionStatusKey struct{}

type compressionStatus struct {
	once   sync.Once
	notify func(context.Context)
}

// WithVideoCompressionStatus reports the transition to compression once per download.
func WithVideoCompressionStatus(ctx context.Context, notify func(context.Context)) context.Context {
	return context.WithValue(ctx, compressionStatusKey{}, &compressionStatus{notify: notify})
}

func notifyVideoCompression(ctx context.Context) {
	if status, ok := ctx.Value(compressionStatusKey{}).(*compressionStatus); ok && status.notify != nil {
		status.once.Do(func() { status.notify(ctx) })
	}
}

// CompressionTimeout includes waiting for the process-wide compression slot.
func (d *Downloader) CompressionTimeout() time.Duration {
	if d == nil || !d.cfg.VideoCompressionEnabled {
		return 0
	}
	if d.cfg.VideoCompressionTimeout > 0 {
		return d.cfg.VideoCompressionTimeout
	}
	return 5 * time.Minute
}

func (d *Downloader) videoSourceLimit() int64 {
	if !d.cfg.VideoCompressionEnabled {
		return d.cfg.MaxFileSize
	}
	limit := d.cfg.VideoMaxSourceSize
	if limit <= 0 {
		limit = 300 * 1024 * 1024
	}
	return max(d.cfg.MaxFileSize, limit)
}

func (d *Downloader) readVideoMedia(ctx context.Context, body io.ReadCloser, length int64, name, contentType string) (Media, error) {
	limit := d.videoSourceLimit()
	if length > limit {
		return Media{}, compressionRefusal("downloader.error.video_source_too_large", limit/(1024*1024))
	}
	file, err := os.CreateTemp("", "nyande-video-source-*")
	if err != nil {
		return Media{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	size, err := io.Copy(file, io.LimitReader(body, limit+1))
	if err != nil {
		return Media{}, err
	}
	if err := file.Close(); err != nil {
		return Media{}, err
	}
	// Release the HTTP connection before waiting for the encoder.
	if err := body.Close(); err != nil {
		return Media{}, err
	}
	if size > limit {
		return Media{}, compressionRefusal("downloader.error.video_source_too_large", limit/(1024*1024))
	}
	return d.videoMediaFromFile(ctx, file.Name(), name, contentType)
}

func (d *Downloader) videoMediaFromFile(ctx context.Context, filename, name, contentType string) (Media, error) {
	if err := ctx.Err(); err != nil {
		return Media{}, err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return Media{}, err
	}
	if info.Size() > d.cfg.MaxFileSize {
		return d.compressVideo(ctx, filename, name)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return Media{}, err
	}
	return Media{Kind: "video", Name: name, MIME: contentType, Data: data}, nil
}
