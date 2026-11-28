package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func TestTelegramOversizedVideoCompressionDelivery(t *testing.T) {
	for _, test := range []struct {
		name, duration, failureKey string
		encoderFails, silent       bool
	}{
		{name: "compressed delivery", duration: "1"},
		{name: "silent compressed delivery", duration: "1", silent: true},
		{name: "long video refusal", duration: "1200", failureKey: "downloader.error.video_too_long"},
		{name: "encoder failure", duration: "1", failureKey: "downloader.error.video_compression_generic", encoderFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			audioStream := `,{"index":1,"codec_type":"audio","codec_name":"aac"}`
			uploadMethod, uploadField := "sendVideo", "video"
			if test.silent {
				audioStream = ""
				uploadMethod, uploadField = "sendAnimation", "animation"
			}
			probeJSON := fmt.Sprintf(`{"format":{"duration":"%s","format_name":"mp4"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264","pix_fmt":"yuv420p","width":320,"height":240}%s]}`, test.duration, audioStream)
			probe := fakeMediaProcess(t, mediaProcessFixture{Kind: "probe", Text: probeJSON})
			encoder := fakeMediaProcess(t, mediaProcessFixture{Kind: "encoder", Text: "compressed-video", Fail: test.encoderFails})
			b := concurrentTestBot()
			b.cfg.MaxFileSize = 64 * 1024
			b.cfg.VideoMaxSourceSize = 256 * 1024
			b.cfg.VideoCompressionEnabled = true
			b.cfg.VideoCompressionTimeout = 3 * time.Second
			b.cfg.VideoMaxDuration = 10 * time.Minute
			b.cfg.FFprobePath, b.cfg.FFmpegPath = probe, encoder
			b.cfg.RetryAttempts = 3
			b.downloader = downloader.New(b.cfg)
			message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}}
			const mediaURL = "https://8.8.8.8/video.mov"
			source := strings.Repeat("original-video", 12000)
			var events []string
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(request *http.Request) (*http.Response, error) {
				body, contentType := `{"ok":true,"result":{"message_id":100}}`, "application/json"
				switch request.URL.Hostname() {
				case "8.8.8.8":
					events = append(events, "download")
					body, contentType = source, "video/quicktime"
				case "api.telegram.org":
					switch filepath.Base(request.URL.Path) {
					case "sendMessage", "editMessageText":
						var payload struct{ Text string }
						if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						events = append(events, payload.Text)
					case "sendVideo", "sendAnimation":
						events = append(events, "upload")
						if test.failureKey != "" {
							t.Fatal("compression failure must not send the original video")
						}
						if method := filepath.Base(request.URL.Path); method != uploadMethod {
							t.Fatalf("compressed media sent with %s, want %s", method, uploadMethod)
						}
						if err := request.ParseMultipartForm(1024 * 1024); err != nil {
							t.Fatal(err)
						}
						defer request.MultipartForm.RemoveAll()
						file, header, err := request.FormFile(uploadField)
						if err != nil {
							t.Fatal(err)
						}
						data, readErr := io.ReadAll(file)
						file.Close()
						if readErr != nil || string(data) != "compressed-video" || int64(len(data)) > b.cfg.MaxFileSize {
							t.Fatalf("unexpected uploaded bytes: size=%d err=%v", len(data), readErr)
						}
						if header.Filename != "video.mp4" || header.Header.Get("Content-Type") != "video/mp4" {
							t.Fatalf("compressed video metadata = %v", header)
						}
						body = `{"ok":true,"result":{"message_id":200}}`
					case "deleteMessage":
						events = append(events, "delete")
						body = `{"ok":true,"result":true}`
					default:
						t.Fatalf("unexpected Telegram request: %s", request.URL.Path)
					}
				default:
					t.Fatalf("unexpected network request: %s", request.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK, ContentLength: int64(len(body)), Request: request,
					Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)),
				}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sent, err := b.downloadAndSendMedia(ctx, message, mediaURL)
			if len(events) == 0 {
				t.Fatal("missing download status")
			}
			smiley, ok := strings.CutPrefix(events[0], strings.TrimSuffix(resources.Get("downloader.status.downloading"), "{smiley}"))
			if !ok || smiley == "" || smiley == "{smiley}" {
				t.Fatalf("unexpected download status: %q", events[0])
			}
			wantEvents := []string{events[0], "download", "Видео большое, сжимаю…"}
			if test.failureKey == "" {
				if err != nil || len(sent) != 1 || sent[0].MessageID != 200 {
					t.Fatalf("delivery = %v, err = %v", sent, err)
				}
				wantEvents = append(wantEvents, resources.Format("downloader.status.sending", map[string]any{"smiley": smiley}), "upload", "delete")
			} else {
				if !errors.Is(err, downloader.ErrVideoCompression) || len(sent) != 0 {
					t.Fatalf("delivery after compression failure = %v, err = %v", sent, err)
				}
				wantMessage := resources.Get(test.failureKey)
				if test.failureKey == "downloader.error.video_too_long" {
					wantMessage = fmt.Sprintf(wantMessage, 10)
				}
				if got := humanDownloadError(mediaURL, err); got != wantMessage {
					t.Fatalf("unsafe or misleading user error = %q, want %q", got, wantMessage)
				}
				wantEvents = append(wantEvents, resources.Format("downloader.error.failed", map[string]any{"error": wantMessage}))
			}
			if !reflect.DeepEqual(events, wantEvents) {
				t.Fatalf("delivery events = %v, want %v", events, wantEvents)
			}
			calls, readErr := os.ReadFile(encoder + ".calls")
			if test.failureKey == "downloader.error.video_too_long" {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("refused video reached encoder: %q, %v", calls, readErr)
				}
			} else if readErr != nil || string(calls) != "called" {
				t.Fatalf("encoder must run once: %q, %v", calls, readErr)
			}
		})
	}
}
