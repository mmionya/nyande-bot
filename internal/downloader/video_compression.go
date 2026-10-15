package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

// ErrVideoCompression marks a terminal compression refusal or failure. Retrying
// the download, or switching extractors, cannot fix these errors.
var ErrVideoCompression = errors.New("video compression failed")

// Encoding is CPU intensive; share a single slot across all Downloader instances.
var videoCompressionSlot = make(chan struct{}, 1)

type videoCompressionError struct {
	message string
	cause   error
}

func (e *videoCompressionError) Error() string {
	if e.cause != nil {
		return e.message + ": " + e.cause.Error()
	}
	return e.message
}

func (e *videoCompressionError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrVideoCompression}
	}
	return []error{ErrVideoCompression, e.cause}
}

// VideoCompressionMessage returns a safe explanation without subprocess output,
// filesystem paths, or other internal diagnostics.
func VideoCompressionMessage(err error) string {
	var failure *videoCompressionError
	if errors.As(err, &failure) {
		return failure.message
	}
	if errors.Is(err, ErrVideoCompression) {
		return resources.Get("downloader.error.video_compression_generic")
	}
	return ""
}

func compressionRefusal(key string, args ...any) error {
	message := resources.Get(key)
	if len(args) > 0 {
		message = fmt.Sprintf(message, args...)
	}
	return &videoCompressionError{message: message}
}

// compressVideo consumes a local source owned by the caller. Its own output and
// subprocess diagnostics are bounded, and temporary output is always removed.
func (d *Downloader) compressVideo(ctx context.Context, filename, name string) (media Media, err error) {
	defer func() {
		if err == nil {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = &videoCompressionError{message: resources.Get("downloader.error.video_compression_timeout"), cause: err}
		} else if !errors.Is(err, ErrVideoCompression) {
			err = &videoCompressionError{message: resources.Get("downloader.error.video_compression_generic"), cause: err}
		}
	}()
	if err := ctx.Err(); err != nil {
		return Media{}, err
	}
	if !d.cfg.VideoCompressionEnabled {
		return Media{}, compressionRefusal("downloader.error.video_compression_disabled")
	}
	if d.cfg.MaxFileSize <= 0 {
		return Media{}, compressionRefusal("downloader.error.video_cannot_fit")
	}
	info, err := os.Stat(filename)
	if err != nil {
		return Media{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Media{}, compressionRefusal("downloader.error.video_invalid")
	}
	sourceLimit := d.videoSourceLimit()
	if info.Size() > sourceLimit {
		return Media{}, compressionRefusal("downloader.error.video_source_too_large", sourceLimit/(1024*1024))
	}
	ctx, cancel := context.WithTimeout(ctx, d.CompressionTimeout())
	defer cancel()
	notifyVideoCompression(ctx)
	select {
	case videoCompressionSlot <- struct{}{}:
		defer func() { <-videoCompressionSlot }()
	case <-ctx.Done():
		return Media{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return Media{}, err
	}
	video, err := d.probeCompressionVideo(ctx, filename)
	if err != nil {
		return Media{}, err
	}
	maxDuration := d.cfg.VideoMaxDuration
	if maxDuration <= 0 {
		maxDuration = 10 * time.Minute
	}
	if video.duration > maxDuration.Seconds() {
		return Media{}, compressionRefusal("downloader.error.video_too_long", int(math.Ceil(maxDuration.Minutes())))
	}
	// Reserve 5% for the MP4 container and bitrate variance. Avoid spending a
	// huge bitrate on a short clip even if the configured upload limit is large.
	totalBitrate := float64(d.cfg.MaxFileSize) * 8 * 0.95 / video.duration
	if math.IsInf(totalBitrate, 0) || math.IsNaN(totalBitrate) {
		return Media{}, compressionRefusal("downloader.error.video_invalid")
	}
	audioBitrate := int64(0)
	if video.hasAudio {
		audioBitrate = 96000
		if totalBitrate < 800000 {
			audioBitrate = 64000
		}
	}
	videoBitrate := int64(math.Min(totalBitrate-float64(audioBitrate), 8000000))
	const minimumVideoBitrate = 150000
	if videoBitrate < minimumVideoBitrate {
		return Media{}, compressionRefusal("downloader.error.video_cannot_fit")
	}
	directory, err := os.MkdirTemp("", "nyande-compress-*")
	if err != nil {
		return Media{}, err
	}
	defer os.RemoveAll(directory)
	output := filepath.Join(directory, "video.mp4")
	for attempt := 0; attempt < 2; attempt++ {
		if err := d.encodeCompressedVideo(ctx, filename, output, video, videoBitrate, audioBitrate); err != nil {
			return Media{}, err
		}
		info, err := os.Stat(output)
		if err != nil {
			return Media{}, err
		}
		if info.Size() == 0 {
			return Media{}, compressionRefusal("downloader.error.video_cannot_fit")
		}
		if info.Size() <= d.cfg.MaxFileSize {
			file, err := os.Open(output)
			if err != nil {
				return Media{}, err
			}
			data, readErr := io.ReadAll(io.LimitReader(file, d.cfg.MaxFileSize+1))
			closeErr := file.Close()
			if err := errors.Join(readErr, closeErr, ctx.Err()); err != nil {
				return Media{}, err
			}
			if int64(len(data)) > d.cfg.MaxFileSize {
				return Media{}, compressionRefusal("downloader.error.video_cannot_fit")
			}
			name = filepath.Base(name)
			name = strings.TrimSuffix(name, filepath.Ext(name))
			if name == "" || name == "." {
				name = "video"
			}
			return Media{Kind: "video", Name: name + ".mp4", MIME: "video/mp4", Data: data}, nil
		}
		// A complete second encode may fix an overshoot. Never truncate the
		// file with -fs or drop the end of the video with -t/-shortest.
		ratio := float64(d.cfg.MaxFileSize) / float64(info.Size()) * 0.90
		videoBitrate = int64(float64(videoBitrate+audioBitrate)*ratio) - audioBitrate
		if videoBitrate < minimumVideoBitrate {
			break
		}
	}
	return Media{}, compressionRefusal("downloader.error.video_cannot_fit")
}

type compressionVideoInfo struct {
	duration   float64
	videoIndex int
	hasAudio   bool
}

func (d *Downloader) probeCompressionVideo(ctx context.Context, filename string) (compressionVideoInfo, error) {
	probePath := d.cfg.FFprobePath
	if probePath == "" {
		probePath = "ffprobe"
	}
	command := exec.CommandContext(ctx, probePath, "-v", "error", "-threads", "1",
		"-protocol_whitelist", "file,pipe", "-show_entries",
		"format=duration:stream=index,codec_type,width,height:stream_disposition=attached_pic",
		"-of", "json", "-i", filename)
	command.WaitDelay = time.Second
	stdout := compressionOutputBuffer{limit: 64 * 1024}
	stderr := compressionOutputBuffer{limit: 4096}
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return compressionVideoInfo{}, ctx.Err()
		}
		return compressionVideoInfo{}, fmt.Errorf(resources.Get("downloader.error.video_probe_failed"), err, stderr.data)
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Index       int    `json:"index"`
			CodecType   string `json:"codec_type"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if stdout.overflow || json.Unmarshal(stdout.data, &probe) != nil {
		return compressionVideoInfo{}, compressionRefusal("downloader.error.video_invalid")
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return compressionVideoInfo{}, compressionRefusal("downloader.error.video_invalid")
	}
	result := compressionVideoInfo{duration: duration, videoIndex: -1}
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" && stream.Disposition.AttachedPic == 0 && stream.Width >= 2 && stream.Height >= 2 && stream.Index >= 0 && result.videoIndex < 0 {
			result.videoIndex = stream.Index
		}
		result.hasAudio = result.hasAudio || stream.CodecType == "audio"
	}
	if result.videoIndex < 0 {
		return compressionVideoInfo{}, compressionRefusal("downloader.error.video_invalid")
	}
	return result, nil
}

func (d *Downloader) encodeCompressedVideo(ctx context.Context, input, output string, video compressionVideoInfo, videoBitrate, audioBitrate int64) error {
	width, height := 1280, 720
	if videoBitrate < 900000 {
		width, height = 854, 480
	}
	if videoBitrate < 450000 {
		width, height = 640, 360
	}
	// Expressions use the dimensions after FFmpeg's automatic rotation, retain
	// the aspect ratio, and never enlarge a smaller input.
	filter := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", width, height)
	arguments := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-threads", "1", "-filter_threads", "1", "-protocol_whitelist", "file,pipe", "-i", input,
		"-map", "0:" + strconv.Itoa(video.videoIndex), "-map", "0:a:0?", "-sn", "-dn",
		"-map_metadata", "-1", "-map_chapters", "-1", "-vf", filter, "-fpsmax", "30",
		"-c:v", "libx264", "-preset", "fast", "-pix_fmt", "yuv420p", "-threads", "1",
		"-b:v", strconv.FormatInt(videoBitrate, 10), "-maxrate", strconv.FormatInt(videoBitrate, 10),
		"-bufsize", strconv.FormatInt(videoBitrate*2, 10)}
	if video.hasAudio {
		arguments = append(arguments, "-c:a", "aac", "-b:a", strconv.FormatInt(audioBitrate, 10), "-ac", "2")
	} else {
		arguments = append(arguments, "-an")
	}
	arguments = append(arguments, "-movflags", "+faststart", "-f", "mp4", output)
	ffmpegPath := d.cfg.FFmpegPath
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	command := exec.CommandContext(ctx, ffmpegPath, arguments...)
	command.WaitDelay = time.Second
	diagnostic := compressionOutputBuffer{limit: 4096}
	command.Stdout, command.Stderr = &diagnostic, &diagnostic
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf(resources.Get("downloader.error.video_compression_failed"), err, diagnostic.data)
	}
	return ctx.Err()
}

// Discard excess subprocess output while continuing to drain its pipe. This
// bounds memory even when a malformed input produces repeated diagnostics.
type compressionOutputBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (b *compressionOutputBuffer) Write(data []byte) (int, error) {
	remaining := max(0, b.limit-len(b.data))
	if len(data) > remaining {
		b.overflow = true
	}
	b.data = append(b.data, data[:min(remaining, len(data))]...)
	return len(data), nil
}
