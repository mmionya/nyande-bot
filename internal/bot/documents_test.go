package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/documents"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestTelegramDocumentsReachLLM(t *testing.T) {
	for _, mode := range []string{"bare", "caption", "reply", "corrupt", "download failure", "too large"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			b.cfg.MaxFileSize = documents.MaxFileSize
			b.cfg.LLMVision = true
			data, err := os.ReadFile("../documents/testdata/sample.docx")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt" {
				data = []byte("invalid")
			}
			calls, downloads := 0, 0
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
						t.Error(err)
					}
					var parts []struct{ Type, Text string }
					if err := json.Unmarshal(payload.Messages[len(payload.Messages)-1].Content, &parts); err != nil {
						t.Error(err)
					}
					if len(parts) != 2 || parts[1].Type != "image_url" || !strings.Contains(parts[0].Text, "Revenue") {
						t.Errorf("document text/image missing: %#v", parts)
					}
					body = []byte(`{"choices":[{"message":{"content":"Summary"}}]}`)
				case strings.HasSuffix(r.URL.Path, "/getFile"):
					body = []byte(`{"ok":true,"result":{"file_path":"documents/sample.docx"}}`)
					if mode == "download failure" {
						body = []byte(`{"ok":false,"description":"File unavailable"}`)
					}
				case strings.Contains(r.URL.Path, "/file/"):
					downloads++
					body = data
				case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
					body = []byte(`{"ok":true,"result":true}`)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 1, Type: "private"}, Document: &telegram.Document{FileID: "file", FileName: "sample.docx", MimeType: "application/octet-stream"}}
			if mode == "caption" {
				message.Caption = "Summarize this"
			}
			if mode == "reply" {
				message = &telegram.Message{MessageID: 2, Chat: message.Chat, Text: "Summarize this", ReplyToMessage: message}
			}
			if mode == "too large" {
				message.Document.FileSize = documents.MaxFileSize + 1
			}
			if err := b.handleLLM(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "corrupt" || mode == "download failure" || mode == "too large" {
				want = 0
			}
			if calls != want {
				t.Fatalf("LLM calls=%d want=%d", calls, want)
			}
			if mode == "too large" && downloads != 0 {
				t.Fatal("oversize document downloaded")
			}
		})
	}
}
