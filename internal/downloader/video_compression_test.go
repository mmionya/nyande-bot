package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
)

const validCompressionProbe = `{"format":{"duration":"10"},"streams":[{"index":0,"codec_type":"video","width":1920,"height":1080},{"index":1,"codec_type":"audio"}]}`

func compressionFixture(t *testing.T, probe, encode string) (*Downloader, string, string) {
	t.Helper()
	directory := t.TempDir()
	ffprobe := filepath.Join(directory, "ffprobe")
	ffmpeg := filepath.Join(directory, "ffmpeg")
	input := filepath.Join(directory, "source.mov")
	for filename, body := range map[string]string{
		ffprobe: "#!/bin/sh\nset -eu\nprintf '%s' '" + strings.ReplaceAll(probe, "'", "'\\''") + "'\n",
		ffmpeg:  "#!/bin/sh\nset -eu\nprintf '%s\\n' START \"$@\" >> \"$0.args\"\nfor output do :; done\nprintf '%s\\n' \"$output\" >> \"$0.outputs\"\n" + encode,
	} {
		if err := os.WriteFile(filename, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(2 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return New(config.Config{
		VideoCompressionEnabled: true, VideoMaxSourceSize: 3 * 1024 * 1024,
		VideoCompressionTimeout: 5 * time.Second, VideoMaxDuration: 10 * time.Minute,
		MaxFileSize: 1024 * 1024, FFmpegPath: ffmpeg, FFprobePath: ffprobe,
	}), input, ffmpeg
}

func TestCompressVideoRetriesOvershootWithoutTruncationAndCleansOutput(t *testing.T) {
	d, input, encoder := compressionFixture(t, validCompressionProbe, `
if [ -f "$0.once" ]; then
    head -c 900000 /dev/zero > "$output"
else
    touch "$0.once"
    head -c 1200000 /dev/zero > "$output"
fi
`)
	item, err := d.compressVideo(context.Background(), input, "folder/clip.MOV")
	if err != nil {
		t.Fatal(err)
	}
	if item.Kind != "video" || item.MIME != "video/mp4" || item.Name != "clip.mp4" || len(item.Data) != 900000 {
		t.Fatalf("wrong compressed result: kind=%s mime=%s name=%s bytes=%d", item.Kind, item.MIME, item.Name, len(item.Data))
	}
	data, err := os.ReadFile(encoder + ".args")
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(string(data), "START\n")[1:]
	if len(calls) != 2 {
		t.Fatalf("encode calls=%d, want 2", len(calls))
	}
	var bitrates []int
	for _, call := range calls {
		args := strings.Split(strings.TrimSpace(call), "\n")
		threadCount := 0
		for index, arg := range args {
			switch arg {
			case "-fs", "-t", "-shortest":
				t.Fatalf("encoder may truncate clip: %s", call)
			case "-threads":
				threadCount++
				if args[index+1] != "1" {
					t.Fatal("unbounded codec threads")
				}
			case "-b:v":
				bitrate, err := strconv.Atoi(args[index+1])
				if err != nil {
					t.Fatal(err)
				}
				bitrates = append(bitrates, bitrate)
			}
		}
		if threadCount != 2 || !strings.Contains(call, "-protocol_whitelist\nfile,pipe\n") || !strings.Contains(call, "-c:v\nlibx264\n") || !strings.Contains(call, "-c:a\naac\n") {
			t.Fatalf("missing bounded, compatible encoding options: %s", call)
		}
	}
	if len(bitrates) != 2 || bitrates[1] >= bitrates[0] {
		t.Fatalf("second bitrate must be lower: %v", bitrates)
	}
	outputs, err := os.ReadFile(encoder + ".outputs")
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range strings.Fields(string(outputs)) {
		if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
			t.Fatalf("temporary output directory left behind: %s, %v", output, err)
		}
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("caller-owned input was removed: %v", err)
	}
}

func TestCompressVideoRejectsUnsafeInputsBeforeEncoding(t *testing.T) {
	for _, test := range []struct {
		name, probe string
		configure   func(*Downloader)
	}{
		{"invalid JSON", "{", nil},
		{"nonfinite duration", strings.Replace(validCompressionProbe, `"10"`, `"NaN"`, 1), nil},
		{"infinite duration", strings.Replace(validCompressionProbe, `"10"`, `"+Inf"`, 1), nil},
		{"zero duration", strings.Replace(validCompressionProbe, `"10"`, `"0"`, 1), nil},
		{"negative duration", strings.Replace(validCompressionProbe, `"10"`, `"-1"`, 1), nil},
		{"no video", `{"format":{"duration":"10"},"streams":[{"codec_type":"audio"}]}`, nil},
		{"attached picture only", `{"format":{"duration":"10"},"streams":[{"index":0,"codec_type":"video","width":100,"height":100,"disposition":{"attached_pic":1}}]}`, nil},
		{"too long", strings.Replace(validCompressionProbe, `"10"`, `"601"`, 1), nil},
		{"quality floor", validCompressionProbe, func(d *Downloader) { d.cfg.MaxFileSize = 100000 }},
		{"source too large", validCompressionProbe, func(d *Downloader) { d.cfg.VideoMaxSourceSize = 1024 * 1024 }},
		{"disabled", validCompressionProbe, func(d *Downloader) { d.cfg.VideoCompressionEnabled = false }},
		{"oversized probe output", strings.Repeat(" ", 64*1024) + validCompressionProbe, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, input, encoder := compressionFixture(t, test.probe, "exit 1\n")
			if test.configure != nil {
				test.configure(d)
			}
			_, err := d.compressVideo(context.Background(), input, "clip.mov")
			if !errors.Is(err, ErrVideoCompression) || VideoCompressionMessage(err) == "" {
				t.Fatalf("expected safe compression refusal, got %v", err)
			}
			if _, err := os.Stat(encoder + ".args"); !os.IsNotExist(err) {
				t.Fatalf("encoder ran for an input that should be refused: %v", err)
			}
		})
	}
}

func TestCompressVideoFailureHasBoundedPrivateDiagnostics(t *testing.T) {
	d, input, encoder := compressionFixture(t, validCompressionProbe, `
printf 'private diagnostic: /secret/path' >&2
head -c 100000 /dev/zero >&2
exit 2
`)
	_, err := d.compressVideo(context.Background(), input, "clip.mov")
	if !errors.Is(err, ErrVideoCompression) || len(err.Error()) > 5000 {
		t.Fatalf("unbounded or unclassified failure: %v", err)
	}
	if strings.Contains(VideoCompressionMessage(err), "private") || strings.Contains(VideoCompressionMessage(err), "secret") {
		t.Fatal("internal diagnostics exposed to user")
	}
	outputs, _ := os.ReadFile(encoder + ".outputs")
	output := strings.TrimSpace(string(outputs))
	if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
		t.Fatal("failed encoder left temporary files")
	}
	if VideoCompressionMessage(errors.New("unrelated")) != "" {
		t.Fatal("unrelated errors must not be classified as compression errors")
	}
}

func TestCompressVideoStopsAfterTwoOversizedEncodes(t *testing.T) {
	d, input, encoder := compressionFixture(t, validCompressionProbe, "head -c 1200000 /dev/zero > \"$output\"\n")
	_, err := d.compressVideo(context.Background(), input, "clip.mov")
	if !errors.Is(err, ErrVideoCompression) {
		t.Fatalf("expected refusal, got %v", err)
	}
	data, _ := os.ReadFile(encoder + ".args")
	if calls := strings.Count(string(data), "START\n"); calls != 2 {
		t.Fatalf("encode calls=%d, want 2", calls)
	}
}

func TestCompressVideoQueueIsCancellableAndTimeoutIncludesQueue(t *testing.T) {
	videoCompressionSlot <- struct{}{}
	defer func() { <-videoCompressionSlot }()
	d, input, encoder := compressionFixture(t, validCompressionProbe, "exit 1\n")
	d.cfg.VideoCompressionTimeout = 30 * time.Millisecond
	start := time.Now()
	_, err := d.compressVideo(context.Background(), input, "clip.mov")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrVideoCompression) || time.Since(start) > time.Second {
		t.Fatalf("queue did not honor timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = d.compressVideo(ctx, input, "clip.mov")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrVideoCompression) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	if _, err := os.Stat(encoder + ".args"); !os.IsNotExist(err) {
		t.Fatal("waiting request ran encoder")
	}
}

func TestCompressVideoCancelsRunningEncoderAndReleasesSlot(t *testing.T) {
	d, input, encoder := compressionFixture(t, validCompressionProbe, "exec sleep 30\n")
	d.cfg.VideoCompressionTimeout = 80 * time.Millisecond
	start := time.Now()
	_, err := d.compressVideo(context.Background(), input, "clip.mov")
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrVideoCompression) || time.Since(start) > 2*time.Second {
		t.Fatalf("running encoder did not honor timeout: %v", err)
	}
	if len(videoCompressionSlot) != 0 {
		t.Fatal("encoder did not release global slot")
	}
	outputs, _ := os.ReadFile(encoder + ".outputs")
	if _, err := os.Stat(filepath.Dir(strings.TrimSpace(string(outputs)))); !os.IsNotExist(err) {
		t.Fatal("cancelled encoder left temporary files")
	}
}

func TestCompressVideoRealFFmpegPreservesDurationAndAudio(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	directory := t.TempDir()
	input := filepath.Join(directory, "source.mkv")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-filter_threads", "1",
		"-f", "lavfi", "-i", "testsrc2=size=320x242:rate=60:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=2",
		"-vf", "setsar=4/3", "-c:v", "ffv1", "-threads", "1", "-c:a", "pcm_s16le", input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create input: %v: %s", err, output)
	}
	const maxSize = 180000
	info, err := os.Stat(input)
	if err != nil || info.Size() <= maxSize {
		t.Fatalf("fixture must exceed upload limit: %v, %v", info, err)
	}
	d := New(config.Config{VideoCompressionEnabled: true, MaxFileSize: maxSize,
		VideoCompressionTimeout: 20 * time.Second, FFmpegPath: ffmpeg, FFprobePath: ffprobe})
	item, err := d.compressVideo(ctx, input, "source.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Data) > maxSize || len(item.Data) == 0 {
		t.Fatalf("output size=%d, max=%d", len(item.Data), maxSize)
	}
	output := filepath.Join(directory, "compressed.mp4")
	if err := os.WriteFile(output, item.Data, 0600); err != nil {
		t.Fatal(err)
	}
	probe, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=duration:stream=codec_type,codec_name,width,height,r_frame_rate,sample_aspect_ratio", "-of", "json", output).Output()
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Format  struct{ Duration string } `json:"format"`
		Streams []struct {
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			Width, Height int
			Rate          string `json:"r_frame_rate"`
			Aspect        string `json:"sample_aspect_ratio"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(probe, &decoded); err != nil {
		t.Fatal(err)
	}
	duration, _ := strconv.ParseFloat(decoded.Format.Duration, 64)
	if math.Abs(duration-2) > 0.1 {
		t.Fatalf("clip duration changed: %f", duration)
	}
	video, audio := false, false
	for _, stream := range decoded.Streams {
		switch stream.CodecType {
		case "video":
			var numerator, denominator float64
			_, err := fmt.Sscanf(stream.Rate, "%f/%f", &numerator, &denominator)
			if stream.CodecName != "h264" || stream.Width > 320 || stream.Height > 242 || stream.Width%2 != 0 || stream.Height%2 != 0 || err != nil || denominator <= 0 || numerator/denominator > 30 {
				t.Fatalf("incompatible or enlarged video stream: %+v", stream)
			}
			var aspectX, aspectY float64
			_, err = fmt.Sscanf(stream.Aspect, "%f:%f", &aspectX, &aspectY)
			if err != nil || aspectY <= 0 || math.Abs(float64(stream.Width)*aspectX/(float64(stream.Height)*aspectY)-(320.0*4.0/(242.0*3.0))) > 0.01 {
				t.Fatalf("display aspect ratio changed: %+v", stream)
			}
			video = true
		case "audio":
			if stream.CodecName != "aac" {
				t.Fatalf("incompatible audio stream: %+v", stream)
			}
			audio = true
		}
	}
	if !video || !audio {
		t.Fatalf("video or audio was lost: %s", probe)
	}
}
