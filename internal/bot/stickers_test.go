package bot

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func stickerFixture(t *testing.T, ext string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/sticker." + ext)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStickerRendering(t *testing.T) {
	for _, ext := range []string{"webp", "webm", "tgs"} {
		t.Run(ext, func(t *testing.T) {
			if ext == "webm" {
				for _, bin := range []string{"ffmpeg", "ffprobe"} {
					if _, err := exec.LookPath(bin); err != nil {
						t.Skip(bin + " unavailable")
					}
				}
			}
			if ext == "tgs" {
				if err := exec.Command("python3", "-c", "import rlottie_python, PIL").Run(); err != nil {
					t.Skip("rlottie-python[full] unavailable")
				}
			}
			b := concurrentTestBot()
			frames, note, err := b.readSticker(context.Background(), cachedAttachment{Kind: "sticker", Name: "sticker." + ext, Data: stickerFixture(t, ext)})
			want := 3
			if ext == "webp" {
				want = 1
			}
			if err != nil || len(frames) != want || !strings.Contains(note, "Read visible lettering") {
				t.Fatalf("frames=%d note=%q err=%v", len(frames), note, err)
			}
			for _, frame := range frames {
				im, _, err := image.Decode(bytes.NewReader(frame.Data))
				if err != nil {
					t.Fatal(err)
				}
				r, g, b, _ := im.At(10, 10).RGBA()
				if r <= g || r <= b || frame.MIME != "image/png" {
					t.Fatal("actual sticker pixels were not rendered")
				}
			}
		})
	}
}

func TestTGSRejectsUnboundedAndExternalResources(t *testing.T) {
	for _, data := range []string{`{"w":999999,"h":96,"ip":0,"op":30}`, `{"w":96,"h":96,"ip":0,"op":999999}`, `{"w":96,"h":96,"ip":0,"op":30,"assets":[{"p":"/etc/passwd"}]}`, strings.Repeat(" ", 4*1024*1024+1)} {
		var out bytes.Buffer
		w := gzip.NewWriter(&out)
		w.Write([]byte(data))
		w.Close()
		if _, err := renderTGS(context.Background(), out.Bytes(), 3); err == nil {
			t.Fatal("unsafe TGS accepted")
		}
	}
}

func TestTelegramStickerMessages(t *testing.T) {
	for _, mode := range []string{"bare", "reply", "preview", "download preview", "vision disabled", "broken", "download failure", "group unaddressed", "group reply", "group media reply"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.MaxFileSize = 20 * 1024 * 1024
			b.cfg.LLMVision = mode != "vision disabled"
			b.identity.ID = 99
			message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 1, Type: "private"}, Sticker: &telegram.Sticker{FileID: "sticker", Emoji: "MISLEADING_EMOJI_HINT"}}
			if mode == "reply" {
				message = &telegram.Message{MessageID: 2, Chat: message.Chat, Text: "Что написано?", ReplyToMessage: message}
			}
			if mode == "preview" || mode == "download preview" {
				message.Sticker.IsAnimated = true
				message.Sticker.Thumbnail = &telegram.PhotoSize{FileID: "preview"}
			}
			if strings.HasPrefix(mode, "group") {
				message.Chat.Type = "supergroup"
				if mode == "group reply" || mode == "group media reply" {
					message.ReplyToMessage = &telegram.Message{MessageID: 3, From: &telegram.User{ID: 99}}
				}
				if mode == "group media reply" {
					b.media.Put(1, []telegram.Message{{MessageID: 3}}, []cachedAttachment{{Kind: "photo", Data: []byte("cached image must not replace current sticker")}})
				}
			}
			calls, downloads := 0, 0
			var sent string
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := []byte(`{"ok":true,"result":{"message_id":100}}`)
				switch {
				case r.URL.Hostname() == "llm.test":
					calls++
					var payload struct {
						Messages []struct{ Content json.RawMessage }
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					var parts []struct {
						Type, Text string
						ImageURL   struct{ URL string } `json:"image_url"`
					}
					if err := json.Unmarshal(payload.Messages[len(payload.Messages)-1].Content, &parts); err != nil {
						t.Fatal(err)
					}
					if len(parts) != 2 || parts[1].Type != "image_url" {
						t.Fatalf("missing sticker image: %#v", parts)
					}
					if strings.Contains(parts[0].Text, "MISLEADING_EMOJI_HINT") || !strings.Contains(parts[0].Text, "Read visible lettering") {
						t.Errorf("wrong sticker context: %s", parts[0].Text)
					}
					if mode == "bare" && !strings.Contains(parts[0].Text, resources.Get("llm.prompt.sticker_only")) {
						t.Error("sticker was treated as an ordinary photo")
					}
					if (mode == "preview" || mode == "download preview") && !strings.Contains(parts[0].Text, "Only a static thumbnail") {
						t.Error("thumbnail presented as full animation")
					}
					data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(parts[1].ImageURL.URL, "data:image/png;base64,"))
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
						t.Fatal(err)
					}
					body = []byte(`{"choices":[{"message":{"content":"Привет!"}}]}`)
				case strings.HasSuffix(r.URL.Path, "/getFile"):
					var request struct {
						FileID string `json:"file_id"`
					}
					json.NewDecoder(r.Body).Decode(&request)
					body = []byte(fmt.Sprintf(`{"ok":true,"result":{"file_path":"%s"}}`, request.FileID))
					if request.FileID == "sticker" && (mode == "download preview" || mode == "download failure") {
						body = []byte(`{"ok":false,"error_code":400,"description":"file unavailable"}`)
					}
				case strings.Contains(r.URL.Path, "/file/"):
					downloads++
					body = stickerFixture(t, "webp")
					if mode == "broken" || (mode == "preview" && strings.HasSuffix(r.URL.Path, "/sticker")) {
						body = []byte("broken")
					}
				case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
					body = []byte(`{"ok":true,"result":true}`)
				case strings.HasSuffix(r.URL.Path, "/sendMessage"):
					var request struct{ Text string }
					json.NewDecoder(r.Body).Decode(&request)
					sent = request.Text
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			if err := b.handleLLM(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			want := 1
			switch mode {
			case "vision disabled":
				want = 0
				if sent != resources.Get("telegram.sticker.vision_disabled") || downloads != 0 {
					t.Fatalf("disabled vision: %q downloads=%d", sent, downloads)
				}
			case "broken":
				want = 0
				if sent != resources.Get("telegram.sticker.failed") {
					t.Fatal(sent)
				}
			case "download failure":
				want = 0
				if sent != resources.Get("telegram.media.download_failed") {
					t.Fatal(sent)
				}
			case "group unaddressed":
				want = 0
				if sent != "" || downloads != 0 {
					t.Fatal("unaddressed group sticker triggered processing")
				}
			default:
				if sent != "Привет!" {
					t.Fatal(sent)
				}
			}
			if calls != want {
				t.Fatalf("LLM calls=%d want=%d", calls, want)
			}
		})
	}
}
