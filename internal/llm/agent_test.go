package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/resources"
)

type agentTestPayload struct {
	Tools    []map[string]any `json:"tools"`
	Messages []chatMessage    `json:"messages"`
}

func agentTestClient(t *testing.T, handler func(http.ResponseWriter, agentTestPayload)) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload agentTestPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		handler(w, payload)
	}))
	t.Cleanup(server.Close)
	return New(config.Config{LLMEnabled: true, LLMBaseURL: server.URL, LLMTimeout: time.Second, LLMMaxTokens: 2048})
}

func agentTestReply(t *testing.T, w http.ResponseWriter, content string, calls ...toolCall) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop", "message": chatMessage{Role: "assistant", Content: content, ToolCalls: calls},
	}}}); err != nil {
		t.Error(err)
	}
}

func agentTestCall(id, name string, arguments any) toolCall {
	return toolCall{ID: id, Type: "function", Function: toolFunction{Name: name, Arguments: arguments}}
}

func TestAgentExtendsStepsAndIsolatesHistory(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		t.Run(fmt.Sprint(isolated), func(t *testing.T) {
			requests, executed := 0, 0
			var progress []AgentProgress
			client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
				requests++
				encoded, _ := json.Marshal(payload.Messages)
				if strings.Contains(string(encoded), "old-private-conversation") == isolated {
					t.Error("conversation history isolation is wrong")
				}
				if requests <= 4 && len(payload.Tools) > 0 {
					agentTestReply(t, w, "", agentTestCall(fmt.Sprint(requests), "read", fmt.Sprintf(`{"page":"%d"}`, requests)))
					return
				}
				if !isolated && len(payload.Tools) != 0 {
					t.Error("ordinary chat must force a final answer after three rounds")
				}
				agentTestReply(t, w, "Completed")
			})
			client.history[7] = []chatMessage{{Role: "user", Content: "old-private-conversation"}, {Role: "assistant", Content: "old reply"}}
			before := append([]chatMessage(nil), client.history[7]...)
			request := Request{ChatID: 7, Text: "Read four sources", Tools: []Tool{{Name: "read", Execute: func(context.Context, map[string]string) (string, error) {
				executed++
				return "source text", nil
			}}}}
			if isolated {
				request.Agent = &AgentOptions{OnProgress: func(p AgentProgress) { progress = append(progress, p) }}
				client.historyErr = errors.New("history unavailable")
			}
			answer, err := client.Ask(context.Background(), request)
			if err != nil || answer != "Completed" {
				t.Fatalf("answer=%q err=%v", answer, err)
			}
			if isolated {
				if requests != 5 || executed != 4 || !reflect.DeepEqual(before, client.history[7]) {
					t.Fatalf("requests=%d executed=%d history=%#v", requests, executed, client.history[7])
				}
				if len(progress) != 9 || progress[0].Phase != "thinking" || progress[1].Tool != "read" || progress[8].ToolCalls != 4 {
					t.Fatalf("bad progress: %#v", progress)
				}
			} else if requests != 4 || executed != 3 || len(client.history[7]) != 4 {
				t.Fatalf("ordinary behavior changed: requests=%d executed=%d history=%#v", requests, executed, client.history[7])
			}
		})
	}
}

func TestAgentBudgetCoversWholeToolBatch(t *testing.T) {
	requests, executed := 0, 0
	var progress []AgentProgress
	client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
		requests++
		if requests <= 2 {
			count := 2
			if requests == 2 {
				count = 4
			}
			var calls []toolCall
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("%d-%d", requests, i)
				calls = append(calls, agentTestCall(id, "read", map[string]string{"id": id}))
			}
			agentTestReply(t, w, "", calls...)
			return
		}
		if len(payload.Tools) != 0 {
			t.Error("final request still allows tools")
		}
		results, skipped := 0, 0
		for _, message := range payload.Messages {
			if message.Role == "tool" {
				results++
				if message.Content == agentSkippedToolResult {
					skipped++
				}
			}
		}
		if results != 6 || skipped != 3 {
			t.Errorf("missing batch results: results=%d skipped=%d", results, skipped)
		}
		agentTestReply(t, w, "Partial result")
	})
	answer, err := client.Ask(context.Background(), Request{ChatID: 7, Text: "Research", Agent: &AgentOptions{
		MaxToolCalls: 3, OnProgress: func(p AgentProgress) { progress = append(progress, p) },
	}, Tools: []Tool{{Name: "read", Execute: func(context.Context, map[string]string) (string, error) {
		executed++
		return "read result", nil
	}}}})
	if err != nil || requests != 3 || executed != 3 || !strings.Contains(answer, "лимит вызовов инструментов (3)") || len(client.history) != 0 {
		t.Fatalf("answer=%q err=%v requests=%d executed=%d history=%#v", answer, err, requests, executed, client.history)
	}
	last := progress[len(progress)-1]
	if last.Phase != "finalizing" || last.ToolCalls != 3 || last.Step != 2 {
		t.Fatalf("bad final progress: %#v", last)
	}
}

func TestAgentCachesRepeatedResultsAndErrors(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprint(fails), func(t *testing.T) {
			requests, executed := 0, 0
			var firstResult string
			client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
				requests++
				if requests >= 2 {
					result, _ := payload.Messages[len(payload.Messages)-1].Content.(string)
					if requests == 2 {
						firstResult = result
					} else if result != firstResult {
						t.Error("repeated call did not reuse the exact observation")
					}
					if !utf8.ValidString(result) || len([]rune(result)) > maxToolResultRunes || (!fails && !strings.Contains(result, "truncated")) {
						t.Errorf("unbounded or invalid result: runes=%d", len([]rune(result)))
					}
				}
				if requests <= 2 {
					arguments := `{"filename":"report.txt","content":"text"}`
					if requests == 2 {
						arguments = `{ "content": "text", "filename": "report.txt" }`
					}
					agentTestReply(t, w, "", agentTestCall(fmt.Sprint(requests), "create_file", arguments))
					return
				}
				agentTestReply(t, w, "Done")
			})
			answer, err := client.Ask(context.Background(), Request{Text: "Create a file", Agent: &AgentOptions{}, Tools: []Tool{{Name: "create_file", Execute: func(context.Context, map[string]string) (string, error) {
				executed++
				if fails {
					return "", errors.New("delivery failed")
				}
				return strings.Repeat("я😀", maxToolResultRunes), nil
			}}}})
			if err != nil || answer != "Done" || executed != 1 || requests != 3 {
				t.Fatalf("answer=%q err=%v executed=%d requests=%d", answer, err, executed, requests)
			}
		})
	}
}

func TestAgentCancellationStopsLaterToolsAndHistory(t *testing.T) {
	for _, agent := range []bool{false, true} {
		for _, cancelAt := range []string{"before", "thinking", "tool", "executing", "finalizing", "answer"} {
			if !agent && (cancelAt == "thinking" || cancelAt == "tool" || cancelAt == "finalizing") {
				continue
			}
			t.Run(fmt.Sprintf("agent=%t/%s", agent, cancelAt), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				requests, executed := 0, 0
				client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
					requests++
					if cancelAt == "answer" {
						cancel()
						agentTestReply(t, w, "Must not save")
						return
					}
					if requests > 1 {
						t.Error("unexpected completion after cancellation")
						agentTestReply(t, w, "Unexpected")
						return
					}
					agentTestReply(t, w, "", agentTestCall("a", "act", `{"n":"1"}`), agentTestCall("b", "act", `{"n":"2"}`))
				})
				request := Request{ChatID: 7, Text: "Act", Tools: []Tool{{Name: "act", Execute: func(context.Context, map[string]string) (string, error) {
					executed++
					if cancelAt == "executing" {
						cancel()
					}
					return "done", nil
				}}}}
				if agent {
					request.Agent = &AgentOptions{MaxSteps: 1, OnProgress: func(p AgentProgress) {
						if p.Phase == cancelAt {
							cancel()
						}
					}}
				}
				if cancelAt == "before" {
					cancel()
				}
				answer, err := client.Ask(ctx, request)
				wantExecuted := 0
				if cancelAt == "executing" {
					wantExecuted = 1
				} else if cancelAt == "finalizing" {
					wantExecuted = 2
				}
				if answer != "" || !errors.Is(err, context.Canceled) || executed != wantExecuted || len(client.history) != 0 {
					t.Fatalf("answer=%q err=%v executed=%d history=%#v", answer, err, executed, client.history)
				}
			})
		}
	}
}

func TestUnadvertisedToolsNeverExecute(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			requests, executed := 0, 0
			client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
				requests++
				if fallback && requests == 1 {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"tools unsupported"}}`)
					return
				}
				callAt := 1
				if fallback {
					callAt = 2
				}
				if requests == callAt {
					if fallback && len(payload.Tools) != 0 {
						t.Error("fallback must remove advertised tools")
					}
					name := "web_search"
					if fallback {
						name = "act"
					}
					agentTestReply(t, w, "", agentTestCall("blocked", name, `{"query":"cats"}`))
					return
				}
				if payload.Messages[len(payload.Messages)-1].Role != "tool" {
					t.Error("missing rejected-call result")
				}
				agentTestReply(t, w, "Not allowed")
			})
			originalTransport := client.http.Transport
			if originalTransport == nil {
				originalTransport = http.DefaultTransport
			}
			client.http.Transport = tikTokTransport(func(r *http.Request) (*http.Response, error) {
				if !strings.HasPrefix(r.URL.String(), client.cfg.LLMBaseURL) {
					t.Error("disabled web search made a network request")
					return nil, errors.New("unexpected network request")
				}
				return originalTransport.RoundTrip(r)
			})
			answer, err := client.Ask(context.Background(), Request{Text: "Act", Agent: &AgentOptions{}, Tools: []Tool{{Name: "act", Execute: func(context.Context, map[string]string) (string, error) {
				executed++
				return "done", nil
			}}}})
			if err != nil || answer != "Not allowed" || executed != 0 {
				t.Fatalf("answer=%q err=%v executed=%d", answer, err, executed)
			}
		})
	}
}

func TestAgentHardStepAndCallLimits(t *testing.T) {
	for _, requested := range []int{0, -1, 100} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
			requests, executed := 0, 0
			client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
				requests++
				if len(payload.Tools) > 0 {
					agentTestReply(t, w, "", agentTestCall(fmt.Sprint(requests), "act", map[string]int{"n": requests}))
					return
				}
				agentTestReply(t, w, "Partial")
			})
			options := &AgentOptions{MaxSteps: requested, MaxToolCalls: requested}
			answer, err := client.Ask(context.Background(), Request{Text: "Act", Agent: options, Tools: []Tool{{Name: "act", Execute: func(context.Context, map[string]string) (string, error) {
				executed++
				return "done", nil
			}}}})
			if err != nil || requests != 13 || executed != 12 || !strings.Contains(answer, "лимит шагов (12)") {
				t.Fatalf("answer=%q err=%v requests=%d executed=%d", answer, err, requests, executed)
			}
			// The independent call cap must hold even when many calls fit one round.
			run := newAgentRun(options)
			if run.maxCalls != 36 {
				t.Fatalf("unexpected call cap %d", run.maxCalls)
			}
		})
	}
}

func TestAgentPreservesHostedSearchAndCompletedToolsOnFallback(t *testing.T) {
	for _, failAt := range []int{0, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			requests, executed := 0, 0
			client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
				requests++
				hosted := false
				for _, tool := range payload.Tools {
					hosted = hosted || tool["type"] == "openrouter:web_search"
				}
				wantHosted := failAt == 0 || requests <= failAt
				if hosted != wantHosted || !advertisesFunction(payload.Tools, "read") || (!wantHosted && !advertisesFunction(payload.Tools, "web_search")) {
					t.Errorf("unexpected advertised tools at request %d: %#v", requests, payload.Tools)
				}
				observed := 0
				for _, message := range payload.Messages {
					if message.Role == "tool" {
						observed++
						if message.Content != fmt.Sprintf("result %d", observed) {
							t.Errorf("lost observation %d: %#v", observed, message)
						}
					}
				}
				if observed != executed {
					t.Errorf("missing completed work: observed=%d executed=%d", observed, executed)
				}
				if requests == failAt {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"Server tool request failed"}}`)
					return
				}
				if executed < 4 {
					agentTestReply(t, w, "", agentTestCall(fmt.Sprint(executed+1), "read", map[string]int{"page": executed + 1}))
					return
				}
				agentTestReply(t, w, "Done")
			})
			client.cfg.LLMBaseURL += "/openrouter.ai"
			client.cfg.LLMModel = "z-ai/glm-5.3-flash"
			client.cfg.LLMWebSearch = true
			answer, err := client.Ask(context.Background(), Request{Text: "Research", Agent: &AgentOptions{}, Tools: []Tool{{Name: "read", Execute: func(_ context.Context, args map[string]string) (string, error) {
				executed++
				if args["page"] != fmt.Sprint(executed) {
					t.Errorf("replayed or reordered action: executed=%d args=%#v", executed, args)
				}
				return fmt.Sprintf("result %d", executed), nil
			}}}})
			wantRequests := 5
			if failAt > 0 {
				wantRequests++
			}
			if err != nil || answer != "Done" || executed != 4 || requests != wantRequests {
				t.Fatalf("answer=%q err=%v executed=%d requests=%d", answer, err, executed, requests)
			}
		})
	}
}

func TestAgentSearchPromptRequiresSourcesWithoutNoLinksConflict(t *testing.T) {
	for _, agent := range []bool{false, true} {
		t.Run(fmt.Sprint(agent), func(t *testing.T) {
			client := agentTestClient(t, func(w http.ResponseWriter, payload agentTestPayload) {
				if len(payload.Messages) == 0 || payload.Messages[0].Role != "system" {
					t.Error("missing system prompt")
				} else {
					prompt, _ := payload.Messages[0].Content.(string)
					noLinks := resources.Get("llm.prompt.web_search_no_links")
					if strings.Contains(prompt, noLinks) == agent {
						t.Errorf("no-links instruction enabled=%t for agent=%t", strings.Contains(prompt, noLinks), agent)
					}
					if strings.Contains(prompt, "Cite the actual sources used with Markdown links") != agent {
						t.Errorf("citation instruction missing or ordinary prompt changed for agent=%t", agent)
					}
				}
				agentTestReply(t, w, "Done")
			})
			client.cfg.LLMWebSearch = true
			request := Request{Text: "Find sources"}
			if agent {
				request.Agent = &AgentOptions{}
			}
			if _, err := client.Ask(context.Background(), request); err != nil {
				t.Fatal(err)
			}
		})
	}
}
