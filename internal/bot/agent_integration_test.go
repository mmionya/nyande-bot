package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestAgentDocumentToFileFlowUsesScopedToolsAndFormatting(t *testing.T) {
	b := concurrentTestBot()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	requests, uploads, downloads := 0, 0, 0
	typing := make(chan struct{}, 1)
	var final formattedAnswerRequest
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true,"result":{"message_id":99}}`
		switch {
		case r.URL.Hostname() == "llm.test":
			if requests == 0 {
				select {
				case <-typing:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			requests++
			var payload struct {
				Messages []struct{ Role, Content string }
				Tools    []struct{ Function struct{ Name string } }
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if requests == 1 {
				user := payload.Messages[len(payload.Messages)-1].Content
				if !strings.Contains(user, "Revenue: 120") || !strings.Contains(user, "Prepare the report") {
					t.Errorf("task and reply document missing: %q", user)
				}
				allowed := map[string]bool{"current_time": true, "read_webpage": true, "create_file": true}
				for _, tool := range payload.Tools {
					if !allowed[tool.Function.Name] {
						t.Errorf("unexpected agent capability: %s", tool.Function.Name)
					}
					delete(allowed, tool.Function.Name)
				}
				if len(allowed) != 0 {
					t.Errorf("missing tools: %v", allowed)
				}
			}
			assistant := map[string]any{"content": "**Готово**: `report.csv` отправлен. *довольно мурчит*"}
			if requests <= 4 {
				name, args := "current_time", `{}`
				if requests == 1 {
					name, args = "set_reminder", `{"text":"not authorized","delay_seconds":5}`
				}
				if requests == 4 {
					name, args = "create_file", `{"filename":"report.csv","content":"metric,value\nRevenue,120\n","chat_id":"999"}`
				}
				assistant = map[string]any{"tool_calls": []any{map[string]any{"id": fmt.Sprint(requests), "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}
			} else if uploads != 1 {
				t.Errorf("final answer before confirmed upload: %d", uploads)
			}
			encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": assistant}}})
			body = string(encoded)
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			body = `{"ok":true,"result":{"file_path":"documents/input.txt"}}`
		case strings.Contains(r.URL.Path, "/file/"):
			downloads++
			body = "Revenue: 120"
		case strings.HasSuffix(r.URL.Path, "/sendDocument"):
			uploads++
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Error(err)
				break
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("document")
			if err != nil {
				t.Error(err)
				break
			}
			data, err := io.ReadAll(file)
			file.Close()
			if err != nil || header.Filename != "report.csv" || string(data) != "metric,value\nRevenue,120\n" || r.FormValue("chat_id") != "42" {
				t.Errorf("wrong file or delivery scope: %q, %q, %v", header.Filename, data, err)
			}
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if err := json.NewDecoder(r.Body).Decode(&final); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			select {
			case typing <- struct{}{}:
			default:
			}
			body = `{"ok":true,"result":true}`
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 42, Type: "supergroup"}, From: &telegram.User{ID: 7}, Text: "Prepare the report"}
	message.ReplyToMessage = &telegram.Message{MessageID: 16, Chat: message.Chat, Document: &telegram.Document{FileID: "input", FileName: "input.txt", MimeType: "text/plain"}}
	if err := b.handleLLMRequest(context.Background(), message, &llm.AgentOptions{}); err != nil {
		t.Fatal(err)
	}
	if requests != 5 || uploads != 1 || downloads != 1 {
		t.Fatalf("requests=%d uploads=%d downloads=%d", requests, uploads, downloads)
	}
	if final.ChatID != 42 || !strings.Contains(final.Text, "*довольно мурчит*") || len(final.Entities) != 2 || final.Entities[0].Type != "bold" || final.Entities[1].Type != "code" {
		t.Fatalf("final formatting changed: %+v", final)
	}
}
