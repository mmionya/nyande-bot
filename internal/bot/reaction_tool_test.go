package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestTelegramLLMReactions(t *testing.T) {
	for _, mode := range []string{"current", "reply", "text tool", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			b := concurrentTestBot()
			message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: "Поставь сердечко"}
			target, wantID := "current", 17
			if mode == "reply" {
				target, wantID = "reply", 16
				message.ReplyToMessage = &telegram.Message{MessageID: 16, Text: "Спасибо"}
			}
			requests, reactions, answers := 0, 0, 0
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"ok":true,"result":{"message_id":99}}`
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
						found := false
						for _, tool := range payload.Tools {
							found = found || tool.Function.Name == "react_to_message"
						}
						if !found {
							t.Error("reaction tool not advertised")
						}
						args, _ := json.Marshal(map[string]string{"emoji": "❤️", "target": target})
						call := map[string]any{"id": "reaction-1", "type": "function", "function": map[string]any{"name": "react_to_message", "arguments": string(args)}}
						assistant := map[string]any{"tool_calls": []any{call, call}}
						if mode == "text tool" {
							assistant = map[string]any{"content": `<tool_call>{"name":"react_to_message","arguments":` + string(args) + `}</tool_call>`}
						}
						encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": assistant}}})
						body = string(encoded)
					} else {
						observations := 0
						for _, m := range payload.Messages {
							if m.Role == "tool" {
								observations++
								want := "successfully set"
								if mode == "forbidden" {
									want = "not confirmed"
								}
								if !strings.Contains(m.Content, want) {
									t.Errorf("wrong tool outcome: %q", m.Content)
								}
							}
						}
						if observations == 0 || reactions != 1 {
							t.Errorf("tool did not execute exactly once: observations=%d reactions=%d", observations, reactions)
						}
						body = `{"choices":[{"message":{"content":"Готово"}}]}`
					}
				case strings.HasSuffix(r.URL.Path, "/setMessageReaction"):
					reactions++
					var payload struct {
						ChatID    int64 `json:"chat_id"`
						MessageID int   `json:"message_id"`
						Reaction  []struct{ Type, Emoji string }
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.ChatID != 1 || payload.MessageID != wantID || len(payload.Reaction) != 1 || payload.Reaction[0].Type != "emoji" || payload.Reaction[0].Emoji != "❤" {
						t.Errorf("wrong API reaction: %#v", payload)
					}
					body = `{"ok":true,"result":true}`
					if mode == "forbidden" {
						status, body = 403, `{"ok":false,"error_code":403,"description":"REACTION_INVALID"}`
					}
				case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
					body = `{"ok":true,"result":true}`
				case strings.HasSuffix(r.URL.Path, "/sendMessage"):
					answers++
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err := b.handleLLM(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			if requests != 2 || reactions != 1 || answers != 1 {
				t.Fatal(fmt.Sprintf("requests=%d reactions=%d answers=%d", requests, reactions, answers))
			}
		})
	}
}

func TestTelegramReactionCannotTargetAnotherChat(t *testing.T) {
	b := concurrentTestBot()
	tool := b.reactionTool(&telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 1}, ReplyToMessage: &telegram.Message{MessageID: 2, Chat: telegram.Chat{ID: 999}}})
	result, err := tool.Execute(context.Background(), map[string]string{"target": "reply", "emoji": "👍"})
	if err != nil || !strings.Contains(result, "not sent") {
		t.Fatalf("cross-chat reply accepted: %q %v", result, err)
	}
}
