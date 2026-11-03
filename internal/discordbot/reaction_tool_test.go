package discordbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
)

func TestDiscordLLMReactions(t *testing.T) {
	for _, mode := range []string{"current", "reply", "reference only", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			b, err := New(config.Config{DiscordToken: "test", DiscordPrefix: "!", LLMEnabled: true, LLMBaseURL: "https://llm.test", LLMTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			message := &discordgo.Message{ID: "17", ChannelID: "123", Content: "Поставь сердечко", Author: &discordgo.User{ID: "2", Username: "alice"}}
			target, wantID := "current", "17"
			if mode == "reply" {
				target, wantID = "reply", "16"
				message.ReferencedMessage = &discordgo.Message{ID: "16", ChannelID: "123"}
			}
			if mode == "reference only" {
				target, wantID = "reply", "16"
				message.MessageReference = &discordgo.MessageReference{MessageID: "16", ChannelID: "123"}
			}
			requests, reactions, answers := 0, 0, 0
			transport := discordRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"id":"99"}`
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
							t.Error("tool not advertised")
						}
						args, _ := json.Marshal(map[string]string{"emoji": "❤", "target": target})
						call := map[string]any{"id": "reaction-1", "type": "function", "function": map[string]any{"name": "react_to_message", "arguments": string(args)}}
						encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{call, call}}}}})
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
									t.Errorf("wrong outcome: %q", m.Content)
								}
							}
						}
						if observations != 2 || reactions != 1 {
							t.Errorf("observations=%d reactions=%d", observations, reactions)
						}
						body = `{"choices":[{"message":{"content":"Готово"}}]}`
					}
				case r.Method == http.MethodPut:
					reactions++
					if r.URL.Path != "/api/v9/channels/123/messages/"+wantID+"/reactions/❤️/@me" {
						t.Errorf("wrong reaction endpoint: %s", r.URL.Path)
					}
					status, body = 204, ""
					if mode == "forbidden" {
						status, body = 403, `{"message":"Missing Permissions","code":50013}`
					}
				case strings.HasSuffix(r.URL.Path, "/messages/16"):
					body = `{"id":"16","channel_id":"123"}`
				case strings.HasSuffix(r.URL.Path, "/typing"):
				case strings.HasSuffix(r.URL.Path, "/messages"):
					answers++
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			previous := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() { http.DefaultTransport = previous })
			b.session.Client = &http.Client{Transport: transport}
			b.handleLLM(b.session, message)
			if requests != 2 || reactions != 1 || answers != 1 {
				t.Fatalf("requests=%d reactions=%d answers=%d", requests, reactions, answers)
			}
		})
	}
}

func TestDiscordReactionRejectsCrossChannelAndForwardTargets(t *testing.T) {
	for _, ref := range []*discordgo.MessageReference{{MessageID: "16", ChannelID: "other"}, {MessageID: "16", ChannelID: "123", Type: discordgo.MessageReferenceTypeForward}} {
		tool := reactionTool(nil, &discordgo.Message{ID: "17", ChannelID: "123", MessageReference: ref})
		result, err := tool.Execute(context.Background(), map[string]string{"target": "reply", "emoji": "👍"})
		if err != nil || !strings.Contains(result, "not sent") {
			t.Fatalf("unsafe target accepted: %q %v", result, err)
		}
	}
}
