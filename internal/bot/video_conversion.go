package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

type videoProbe struct {
	HasVideo    bool
	HasAudio    bool
	VideoCodec  string
	AudioCodec  string
	PixelFormat string
	FormatName  string
}

var errVideoTooLarge = errors.New(resources.Get("telegram.video.too_large"))

func (b *Bot) videoFileLimit() int64 {
	const telegramLimit = 50 * 1024 * 1024
	if b.cfg.MaxFileSize > 0 && b.cfg.MaxFileSize < telegramLimit {
		return b.cfg.MaxFileSize
	}
	return telegramLimit
}

// Restrict input demuxers as well as protocols: downloaded bytes must never
// cause ffprobe/ffmpeg to follow playlist URLs or read playlist file references.
const mediaInputFormats = videoInputFormats + ",mp3,wav,aac,flac,aiff,amr,ape,ac3,eac3,wv,au"

const videoInputFormats = "mov,matroska,webm,gif,avi,mpegts,mpegvideo,mpeg,ogg,flv,apng,asf"

func (b *Bot) probeVideo(ctx context.Context, data []byte, name string) (videoProbe, error) {
	if err := ctx.Err(); err != nil {
		return videoProbe{}, err
	}
	if int64(len(data)) > b.videoFileLimit() {
		return videoProbe{}, errVideoTooLarge
	}
	directory, err := os.MkdirTemp("", "nyande-video-probe-*")
	if err != nil {
		return videoProbe{}, err
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "input"+safeExtension(name, ".bin"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return videoProbe{}, err
	}
	return b.probeVideoFile(ctx, input)
}

func (b *Bot) probeVideoFile(ctx context.Context, input string) (videoProbe, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, firstString(b.cfg.FFprobePath, "ffprobe"),
		"-v", "error", "-protocol_whitelist", "file,pipe", "-format_whitelist", videoInputFormats,
		"-show_entries", "stream=codec_type,codec_name,pix_fmt:stream_disposition=attached_pic:format=format_name",
		"-of", "json", input,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		if probeCtx.Err() != nil {
			return videoProbe{}, probeCtx.Err()
		}
		return videoProbe{}, fmt.Errorf(resources.Get("telegram.error.video_probe"), err, strings.TrimSpace(string(output)))
	}
	var result struct {
		Streams []struct {
			CodecType   string `json:"codec_type"`
			CodecName   string `json:"codec_name"`
			PixelFormat string `json:"pix_fmt"`
			Disposition struct {
				AttachedPicture int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Name string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return videoProbe{}, fmt.Errorf(resources.Get("telegram.error.video_probe"), err, "")
	}
	info := videoProbe{FormatName: result.Format.Name}
	for _, stream := range result.Streams {
		switch stream.CodecType {
		case "video":
			if !info.HasVideo && stream.Disposition.AttachedPicture == 0 {
				info.HasVideo, info.VideoCodec, info.PixelFormat = true, stream.CodecName, stream.PixelFormat
			}
		case "audio":
			if !info.HasAudio {
				info.HasAudio, info.AudioCodec = true, stream.CodecName
			}
		}
	}
	return info, nil
}

// prepareVideoMP4 preserves the entire video and copies compatible streams.
// silent is used only for Telegram animations; ordinary video retains audio.
func (b *Bot) prepareVideoMP4(ctx context.Context, data []byte, name string, silent bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := b.videoFileLimit()
	if int64(len(data)) > limit {
		return nil, errVideoTooLarge
	}
	directory, err := os.MkdirTemp("", "nyande-video-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "input"+safeExtension(name, ".bin"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}
	info, err := b.probeVideoFile(ctx, input)
	if err != nil {
		return nil, err
	}
	if !info.HasVideo {
		return nil, errors.New(resources.Get("telegram.error.video_missing"))
	}
	output := filepath.Join(directory, "video.mp4")
	arguments := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-protocol_whitelist", "file,pipe", "-format_whitelist", videoInputFormats,
		"-i", input, "-map", "0:V:0",
	}
	if info.VideoCodec == "h264" && info.PixelFormat == "yuv420p" {
		arguments = append(arguments, "-c:v", "copy")
	} else {
		arguments = append(arguments, "-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-threads", "1",
			"-pix_fmt", "yuv420p", "-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2")
	}
	if silent || !info.HasAudio {
		arguments = append(arguments, "-an")
	} else {
		arguments = append(arguments, "-map", "0:a:0")
		if info.AudioCodec == "aac" {
			arguments = append(arguments, "-c:a", "copy")
		} else {
			arguments = append(arguments, "-c:a", "aac", "-b:a", "192k")
		}
	}
	arguments = append(arguments, "-movflags", "+faststart", "-y", output)
	execCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(execCtx, firstString(b.cfg.FFmpegPath, "ffmpeg"), arguments...)
	type commandResult struct {
		output []byte
		err    error
	}
	done := make(chan commandResult, 1)
	go func() {
		output, err := command.CombinedOutput()
		done <- commandResult{output: output, err: err}
	}()
	// Do not use ffmpeg -fs: it can return success with a truncated video.
	// Cancel oversized output and reject it instead of delivering part of it.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if info, err := os.Stat(output); err == nil && info.Size() > limit {
				cancel()
				<-done
				return nil, errVideoTooLarge
			}
		case result := <-done:
			if result.err != nil {
				if execCtx.Err() != nil {
					return nil, execCtx.Err()
				}
				return nil, fmt.Errorf(resources.Get("telegram.error.video_conversion"), result.err, strings.TrimSpace(string(result.output)))
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			info, err := os.Stat(output)
			if err != nil {
				return nil, err
			}
			if info.Size() > limit {
				return nil, errVideoTooLarge
			}
			return os.ReadFile(output)
		}
	}
}
