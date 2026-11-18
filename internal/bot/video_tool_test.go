package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func TestVideoToolRejectsMissingOversizeAndCanceledInput(t *testing.T) {
	for _, test := range []struct {
		name        string
		attachments []cachedAttachment
		err         error
		want        string
	}{
		{"missing", nil, nil, resources.Get("telegram.video.usage")},
		{"photo", []cachedAttachment{{Kind: "photo", MIME: "image/jpeg"}}, nil, resources.Get("telegram.video.usage")},
		{"Telegram limit", nil, errors.New("file is too big"), resources.Get("telegram.video.too_large")},
		{"download failed", nil, errors.New("attachment unavailable"), "attachment unavailable"},
		{"cached limit", []cachedAttachment{{Kind: "animation", Data: []byte("too much data")}}, nil, resources.Get("telegram.video.too_large")},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.MaxFileSize = 1
			tool := b.videoTool(&telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1}}, test.attachments, test.err)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := tool.Execute(ctx, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled call returned %v", err)
			}
			result, err := tool.Execute(context.Background(), nil)
			if err != nil || !strings.Contains(result, test.want) || strings.Contains(result, resources.Get("tool.convert_to_video.sent")) {
				t.Fatalf("result=%q err=%v", result, err)
			}
			repeat, err := tool.Execute(context.Background(), nil)
			if err != nil || !strings.Contains(repeat, result) || !strings.Contains(repeat, "already attempted") {
				t.Fatalf("failure was retried: result=%q err=%v", repeat, err)
			}
		})
	}
	b := concurrentTestBot()
	b.cfg.MaxFileSize = 100 << 20
	if b.videoFileLimit() != 50<<20 {
		t.Fatal("Telegram's 50 MiB limit must apply even when MAX_FILE_SIZE is larger")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.prepareVideoMP4(ctx, nil, "clip.mp4", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled conversion returned %v", err)
	}
	if _, err := b.probeVideo(ctx, nil, "clip.mp4"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe returned %v", err)
	}
}

func TestVideoCommandAndToolSendFreshVideoAndCacheIt(t *testing.T) {
	video := gifTestVideo(t)
	for _, mode := range []string{"cached reply", "current attachment", "tool", "ambiguous upload"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.MaxFileSize = 2 << 20
			message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: "/video", ReplyToMessage: &telegram.Message{MessageID: 16, Caption: "Post text\n\nAuthor"}}
			attachment := cachedAttachment{Kind: "animation", Name: "clip.mp4", MIME: "video/mp4", Data: video}
			b.media.Put(1, []telegram.Message{{MessageID: 16}}, []cachedAttachment{attachment})
			if mode == "current attachment" {
				message.Text, message.Caption = "", "/video"
				message.Animation = &telegram.Animation{FileID: "current", FileName: "clip.mp4", MimeType: "video/mp4", FileSize: int64(len(video))}
				// Choosing the reply instead of current media would fail conversion.
				b.media.Put(1, []telegram.Message{{MessageID: 16}}, []cachedAttachment{{Kind: "animation", Data: []byte("invalid reply")}})
			}
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			uploads, downloads := 0, 0
			var uploaded []byte
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":{"message_id":99}}`
				switch {
				case strings.HasSuffix(r.URL.Path, "/getFile") && mode == "current attachment":
					body = `{"ok":true,"result":{"file_path":"current.mp4"}}`
				case strings.Contains(r.URL.Path, "/file/") && mode == "current attachment":
					downloads++
					body = string(video)
				case strings.HasSuffix(r.URL.Path, "/sendVideo"):
					uploads++
					if err := r.ParseMultipartForm(2 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					file, header, err := r.FormFile("video")
					if err != nil {
						t.Fatal(err)
					}
					uploaded, err = io.ReadAll(file)
					file.Close()
					if err != nil || header.Filename != "clip.mp4" || header.Header.Get("Content-Type") != "video/mp4" {
						t.Fatalf("wrong video upload: %v %v", header, err)
					}
					var reply struct {
						MessageID int `json:"message_id"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != 17 || r.FormValue("chat_id") != "1" || r.FormValue("supports_streaming") != "true" {
						t.Fatalf("wrong video destination: reply=%#v err=%v", reply, err)
					}
					wantCaption := message.ReplyToMessage.Caption
					if mode == "current attachment" {
						wantCaption = ""
					}
					if r.FormValue("caption") != wantCaption {
						t.Fatalf("video caption=%q, want %q", r.FormValue("caption"), wantCaption)
					}
					if mode == "ambiguous upload" {
						body = `{"ok":true,"result":"lost confirmation"}`
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if mode == "tool" || mode == "ambiguous upload" {
				tool := b.videoTool(message, []cachedAttachment{attachment}, nil)
				result, err := tool.Execute(ctx, nil)
				want := resources.Get("tool.convert_to_video.sent")
				if mode == "ambiguous upload" {
					want = "sendVideo"
				}
				if err != nil || !strings.Contains(result, want) {
					t.Fatalf("tool result=%q err=%v", result, err)
				}
				if _, err := tool.Execute(ctx, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			wantDownloads := 0
			if mode == "current attachment" {
				wantDownloads = 1
			}
			if uploads != 1 || downloads != wantDownloads {
				t.Fatalf("uploads=%d downloads=%d", uploads, downloads)
			}
			cached := b.media.Get(1, 99)
			if mode == "ambiguous upload" {
				if len(cached) != 0 {
					t.Fatal("unconfirmed delivery was cached")
				}
			} else if len(cached) != 1 || cached[0].Kind != "video" || !bytes.Equal(cached[0].Data, uploaded) {
				t.Fatal("delivered video was not cached for later conversion")
			}
		})
	}
}

func TestLLMVideoUsesRegisteredToolAndCachedReplyOnce(t *testing.T) {
	video := videoConversionFixture(t, "animation.mp4", "-f", "lavfi", "-i", "color=size=64x48:rate=4",
		"-t", "0.5", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	b := concurrentTestBot()
	b.cfg.MaxFileSize = 2 << 20
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"},
		Text: "Отправь эту гифку обычным видео", ReplyToMessage: &telegram.Message{MessageID: 16}}
	b.media.Put(1, []telegram.Message{{MessageID: 16}}, []cachedAttachment{{Kind: "animation", Name: "animation.mp4", MIME: "video/mp4", Data: video}})
	requests, uploads, answers := 0, 0, 0
	var uploaded []byte
	typing := make(chan struct{}, 4)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true,"result":{"message_id":99}}`
		switch {
		case r.URL.Hostname() == "llm.test":
			requests++
			var payload struct {
				Tools    []struct{ Function struct{ Name string } }
				Messages []struct{ Role, Content string }
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if requests == 1 {
				select {
				case <-typing:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				found := false
				for _, tool := range payload.Tools {
					found = found || tool.Function.Name == "convert_to_video"
				}
				if !found || uploads != 0 {
					t.Fatal("video tool was not advertised before delivery")
				}
				body = `{"choices":[{"message":{"tool_calls":[{"id":"video-1","type":"function","function":{"name":"convert_to_video","arguments":"{}"}},{"id":"video-2","type":"function","function":{"name":"convert_to_video","arguments":"{}"}}]}}]}`
			} else {
				observations := 0
				for _, item := range payload.Messages {
					if item.Role == "tool" {
						observations++
						if !strings.Contains(item.Content, resources.Get("tool.convert_to_video.sent")) {
							t.Fatalf("tool did not report confirmed video delivery: %q", item.Content)
						}
					}
				}
				if observations != 2 || uploads != 1 {
					t.Fatalf("observations=%d uploads=%d", observations, uploads)
				}
				body = `{"choices":[{"message":{"content":"Видео отправлено."}}]}`
			}
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			typing <- struct{}{}
			body = `{"ok":true,"result":true}`
		case strings.HasSuffix(r.URL.Path, "/sendVideo"):
			uploads++
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatal(err)
			}
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("video")
			if err != nil {
				t.Fatal(err)
			}
			uploaded, err = io.ReadAll(file)
			file.Close()
			if err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			answers++
		default:
			t.Errorf("unexpected request; cached reply must not download again: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || uploads != 1 || answers != 1 {
		t.Fatalf("requests=%d uploads=%d answers=%d", requests, uploads, answers)
	}
	info, err := b.probeVideo(ctx, uploaded, "video.mp4")
	if err != nil || !info.HasVideo || info.VideoCodec != "h264" || info.HasAudio {
		t.Fatalf("silent animation must become an ordinary video without artificial audio: %+v %v", info, err)
	}
}

func TestPrepareVideoMP4PreservesPacketsAudioAndFullDuration(t *testing.T) {
	video := videoConversionFixture(t, "source.mp4", "-f", "lavfi", "-i", "color=size=64x48:rate=4",
		"-f", "lavfi", "-i", "sine=frequency=440", "-t", "32", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac")
	b := concurrentTestBot()
	b.cfg.MaxFileSize = 2 << 20
	b.cfg.FFmpegPath, _ = exec.LookPath("ffmpeg")
	b.cfg.FFprobePath, _ = exec.LookPath("ffprobe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, silent := range []bool{false, true} {
		output, err := b.prepareVideoMP4(ctx, video, "source.mp4", silent)
		if err != nil {
			t.Fatal(err)
		}
		info, err := b.probeVideo(ctx, output, "video.mp4")
		if err != nil || !info.HasVideo || info.VideoCodec != "h264" || info.PixelFormat != "yuv420p" || info.HasAudio == silent {
			t.Fatalf("converted stream info=%+v err=%v", info, err)
		}
		if got, want := videoProbeOutput(t, output, "-select_streams", "v", "-show_packets", "-show_entries", "packet=data_hash", "-show_data_hash", "sha256", "-of", "csv=p=0"), videoProbeOutput(t, video, "-select_streams", "v", "-show_packets", "-show_entries", "packet=data_hash", "-show_data_hash", "sha256", "-of", "csv=p=0"); got != want {
			t.Fatal("compatible H.264 packets were changed or truncated")
		}
		duration, err := strconv.ParseFloat(strings.TrimSpace(videoProbeOutput(t, output, "-show_entries", "format=duration", "-of", "csv=p=0")), 64)
		if err != nil || duration < 31.9 {
			t.Fatalf("video duration was truncated: %v %v", duration, err)
		}
	}
}

func TestPrepareVideoMP4ConvertsGIFAndWebM(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"source.gif", []string{"-f", "lavfi", "-i", "testsrc=size=65x49:rate=4", "-t", "1", "-f", "gif"}},
		{"source.webm", []string{"-f", "lavfi", "-i", "color=size=64x48:rate=4", "-t", "1", "-c:v", "libvpx-vp9"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := videoConversionFixture(t, test.name, test.args...)
			b := concurrentTestBot()
			b.cfg.MaxFileSize = 2 << 20
			output, err := b.prepareVideoMP4(context.Background(), input, test.name, false)
			if err != nil {
				t.Fatal(err)
			}
			info, err := b.probeVideo(context.Background(), output, "video.mp4")
			if err != nil || !info.HasVideo || info.VideoCodec != "h264" || info.PixelFormat != "yuv420p" || info.HasAudio || !strings.Contains(info.FormatName, "mp4") {
				t.Fatalf("unexpected converted media: %+v %v", info, err)
			}
			if test.name == "source.gif" && strings.TrimSpace(videoProbeOutput(t, output, "-select_streams", "v", "-show_entries", "stream=width,height", "-of", "csv=p=0")) != "66,50" {
				t.Fatal("odd GIF dimensions were not padded to even H.264 dimensions")
			}
		})
	}
}

func TestVideoConversionRejectsInvalidMediaAndConfiguredMissingProbe(t *testing.T) {
	b := concurrentTestBot()
	b.cfg.FFprobePath = filepath.Join(t.TempDir(), "missing-ffprobe")
	if _, err := b.probeVideo(context.Background(), []byte("invalid media"), "clip.mp4"); err == nil || !strings.Contains(err.Error(), "missing-ffprobe") {
		t.Fatalf("configured probe path was ignored: %v", err)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	b.cfg.FFprobePath = "ffprobe"
	if _, err := b.prepareVideoMP4(context.Background(), []byte("invalid media"), "clip.mp4", false); err == nil {
		t.Fatal("invalid media was accepted")
	}
}

func TestVideoConversionRejectsOversizeOutputAndMissingEncoder(t *testing.T) {
	input := videoConversionFixture(t, "source.webm", "-f", "lavfi", "-i", "color=size=64x48:rate=4", "-t", "1", "-c:v", "libvpx-vp9")
	b := concurrentTestBot()
	b.cfg.MaxFileSize = int64(len(input)) + 1
	if output, err := b.prepareVideoMP4(context.Background(), input, "source.webm", false); !errors.Is(err, errVideoTooLarge) || len(output) != 0 {
		t.Fatalf("oversize conversion returned %d bytes and %v", len(output), err)
	}
	b.cfg.MaxFileSize = 2 << 20
	b.cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing-ffmpeg")
	if _, err := b.prepareVideoMP4(context.Background(), input, "source.webm", false); err == nil || !strings.Contains(err.Error(), "missing-ffmpeg") {
		t.Fatalf("configured encoder path was ignored: %v", err)
	}
}

func videoConversionFixture(t *testing.T, name string, arguments ...string) []byte {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	path := filepath.Join(t.TempDir(), name)
	args := append([]string{"-hide_banner", "-loglevel", "error"}, arguments...)
	args = append(args, "-threads", "1", "-y", path)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create video fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func videoProbeOutput(t *testing.T, data []byte, arguments ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-v", "error"}, arguments...)
	args = append(args, path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ffprobe", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("probe conversion result: %v: %s", err, output)
	}
	return string(output)
}
