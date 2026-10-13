package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func TestLLMGIFUsesCommandConversionAndCachedReply(t *testing.T) {
	video := gifTestVideo(t)
	for _, kind := range []string{"video", "animation", "video_note", "command"} {
		t.Run(kind, func(t *testing.T) {
			b := concurrentTestBot()
			message := &telegram.Message{
				MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"},
				Text: "Сделай из этого гифку", ReplyToMessage: &telegram.Message{MessageID: 16},
			}
			attachmentKind := kind
			if kind == "command" {
				attachmentKind, message.Text = "video", "/gif"
			}
			b.media.Put(message.Chat.ID, []telegram.Message{{MessageID: 16}}, []cachedAttachment{{Kind: attachmentKind, Name: "video.mp4", MIME: "video/mp4", Data: video}})
			requests, uploads, answers := 0, 0, 0
			var animation []byte
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
							found = found || tool.Function.Name == "convert_to_gif"
						}
						if !found || uploads != 0 {
							t.Fatal("GIF tool was not advertised before delivery")
						}
						body = `{"choices":[{"message":{"tool_calls":[{"id":"gif-1","type":"function","function":{"name":"convert_to_gif","arguments":"{}"}},{"id":"gif-2","type":"function","function":{"name":"convert_to_gif","arguments":"{}"}}]}}]}`
					} else {
						observations := 0
						for _, item := range payload.Messages {
							if item.Role == "tool" {
								observations++
								if !strings.Contains(item.Content, resources.Get("tool.convert_to_gif.sent")) {
									t.Fatalf("tool did not report confirmed delivery: %q", item.Content)
								}
							}
						}
						if observations != 2 || uploads != 1 {
							t.Fatalf("observations=%d uploads=%d", observations, uploads)
						}
						body = `{"choices":[{"message":{"content":"Гифка отправлена."}}]}`
					}
				case r.URL.Hostname() == "api.telegram.org" && strings.HasSuffix(r.URL.Path, "/sendChatAction"):
					typing <- struct{}{}
					body = `{"ok":true,"result":true}`
				case r.URL.Hostname() == "api.telegram.org" && strings.HasSuffix(r.URL.Path, "/sendAnimation"):
					uploads++
					if err := r.ParseMultipartForm(2 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					file, header, err := r.FormFile("animation")
					if err != nil {
						t.Fatal(err)
					}
					animation, err = io.ReadAll(file)
					file.Close()
					if err != nil || header.Filename != "animation.mp4" || header.Header.Get("Content-Type") != "video/mp4" {
						t.Fatalf("wrong animation upload: %v %v", header, err)
					}
					var reply struct {
						MessageID int `json:"message_id"`
					}
					if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != message.MessageID || r.FormValue("chat_id") != "1" {
						t.Fatalf("wrong animation destination: chat=%q reply=%#v err=%v", r.FormValue("chat_id"), reply, err)
					}
				case r.URL.Hostname() == "api.telegram.org" && strings.HasSuffix(r.URL.Path, "/sendMessage"):
					answers++
				default:
					t.Errorf("unexpected network request (cached reply must not be downloaded again): %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			if uploads != 1 || (kind == "command" && (requests != 0 || answers != 0)) || (kind != "command" && (requests != 2 || answers != 1)) {
				t.Fatalf("requests=%d uploads=%d answers=%d", requests, uploads, answers)
			}
			path := filepath.Join(t.TempDir(), "animation.mp4")
			if err := os.WriteFile(path, animation, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_type", "-of", "csv=p=0", path).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "h264,video" {
				t.Fatalf("animation must contain only H.264 video, without audio: %s %v", out, err)
			}
		})
	}
}

func TestGIFToolRejectsMissingInputAndDoesNotRepeatFailures(t *testing.T) {
	video := gifTestVideo(t)
	for _, test := range []struct {
		name          string
		attachments   []cachedAttachment
		collectionErr error
		want          string
		uploads       int
	}{
		{"missing", nil, nil, resources.Get("telegram.gif.usage"), 0},
		{"photo", []cachedAttachment{{Kind: "photo", Data: video}}, nil, resources.Get("telegram.gif.usage"), 0},
		{"too large", nil, errors.New("file is too big"), resources.Get("telegram.media.video_too_large"), 0},
		{"collection failed", nil, errors.New("attachment unavailable"), "attachment unavailable", 0},
		{"invalid video", []cachedAttachment{{Kind: "video", Name: "bad.mp4", Data: []byte("not a video")}}, nil, "ffmpeg", 0},
		{"ambiguous delivery", []cachedAttachment{{Kind: "photo"}, {Kind: "video_note", Name: "video.mp4", Data: video}}, nil, "sendAnimation", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			uploads := 0
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "api.telegram.org" || !strings.HasSuffix(r.URL.Path, "/sendAnimation") {
					t.Fatalf("tool must report errors to the model, not send extra messages: %s", r.URL)
				}
				uploads++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":"lost confirmation"}`))}, nil
			})
			b := &Bot{telegram: telegram.New("test-token")}
			tool := b.gifTool(&telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1}}, test.attachments, test.collectionErr)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := tool.Execute(ctx, nil); !errors.Is(err, context.Canceled) || uploads != 0 {
				t.Fatalf("canceled call attempted conversion/delivery: %v uploads=%d", err, uploads)
			}
			result, err := tool.Execute(context.Background(), nil)
			if err != nil || !strings.Contains(result, test.want) || strings.Contains(result, resources.Get("tool.convert_to_gif.sent")) || uploads != test.uploads {
				t.Fatalf("result=%q err=%v uploads=%d", result, err, uploads)
			}
			repeat, err := tool.Execute(context.Background(), nil)
			if err != nil || !strings.Contains(repeat, result) || !strings.Contains(repeat, "already attempted") || uploads != test.uploads {
				t.Fatalf("failed attempt repeated: result=%q err=%v uploads=%d", repeat, err, uploads)
			}
		})
	}
}

func gifTestVideo(t *testing.T) []byte {
	t.Helper()
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	path := filepath.Join(t.TempDir(), "video.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=size=64x48:rate=15", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "0.2", "-threads", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test video: %v: %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
