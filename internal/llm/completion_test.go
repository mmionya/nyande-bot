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

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestLengthRetryPreservesCompletedToolsAndHistory(t *testing.T) {
	requests, executed := 0, 0
	var beforeRetry map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		switch requests {
		case 1:
			fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"file-1","type":"function","function":{"name":"create_file","arguments":"{}"}}]}}]}`)
		case 2:
			beforeRetry = payload
			fmt.Fprint(w, `{"choices":[{"finish_reason":"length","message":{"content":"","reasoning":"PRIVATE reasoning"}}]}`)
		case 3:
			if payload["max_tokens"] != float64(8192) || beforeRetry["max_tokens"] != float64(2048) {
				t.Errorf("unexpected limits: %v -> %v", beforeRetry["max_tokens"], payload["max_tokens"])
			}
			beforeRetry["max_tokens"] = payload["max_tokens"]
			if !reflect.DeepEqual(beforeRetry, payload) {
				t.Error("retry changed completed actions, observations or tools")
			}
			messages, _ := json.Marshal(payload["messages"])
			if !strings.Contains(string(messages), "file sent") || strings.Contains(string(messages), "PRIVATE") {
				t.Errorf("invalid retry context: %s", messages)
			}
			fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"Готовый ответ","reasoning_content":"PRIVATE reasoning"}}]}`)
		default:
			t.Error("unexpected extra request")
		}
	}))
	defer server.Close()
	client := New(config.Config{LLMEnabled: true, LLMBaseURL: server.URL, LLMMaxTokens: 2048, LLMTimeout: time.Second})
	answer, err := client.Ask(context.Background(), Request{ChatID: 7, Text: "Напиши код и объясни подробно", Tools: []Tool{{
		Name: "create_file", Execute: func(context.Context, map[string]string) (string, error) {
			executed++
			return "file sent", nil
		},
	}}})
	if err != nil || answer != "Готовый ответ" || requests != 3 || executed != 1 {
		t.Fatalf("answer=%q err=%v requests=%d executed=%d", answer, err, requests, executed)
	}
	if history := client.history[7]; len(history) != 2 || history[1].Content != answer {
		t.Fatalf("wrong history: %#v", history)
	}
}

func TestLengthLimitNeverExecutesTruncatedTools(t *testing.T) {
	for _, message := range []string{
		`{"reasoning":"PRIVATE","content":""}`,
		`{"content":"<think>PRIVATE"}`,
		`{"content":"Обрезанный ответ"}`,
		`{"tool_calls":[{"id":"1","type":"function","function":{"name":"create_file","arguments":"{\"content\":\"cut"}}]}`,
		`{"content":"<tool_call>create_file<arg_key>content</arg_key><arg_value>cut"}`,
	} {
		t.Run(message, func(t *testing.T) {
			requests, executed := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				fmt.Fprintf(w, `{"choices":[{"finish_reason":"length","message":%s}]}`, message)
			}))
			defer server.Close()
			client := New(config.Config{LLMEnabled: true, LLMBaseURL: server.URL, LLMMaxTokens: 2048, LLMTimeout: time.Second})
			answer, err := client.Ask(context.Background(), Request{ChatID: 7, Text: "Большой файл", Tools: []Tool{{
				Name: "create_file", Execute: func(context.Context, map[string]string) (string, error) {
					executed++
					return "sent", nil
				},
			}}})
			if answer != "" || !errors.Is(err, errAnswerLimit) || requests != 2 || executed != 0 || len(client.history[7]) != 0 {
				t.Fatalf("answer=%q err=%v requests=%d executed=%d history=%#v", answer, err, requests, executed, client.history[7])
			}
		})
	}
}

func TestReasoningIsNeverAnAnswer(t *testing.T) {
	for _, forcedFinal := range []bool{false, true} {
		for _, field := range []string{"reasoning", "reasoning_content"} {
			t.Run(fmt.Sprintf("%s/forced=%t", field, forcedFinal), func(t *testing.T) {
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if forcedFinal && requests <= maxAgentSteps {
						fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"time","type":"function","function":{"name":"current_time","arguments":"{}"}}]}}]}`)
						return
					}
					fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"content":"",%q:"PRIVATE reasoning"}}]}`, field)
				}))
				defer server.Close()
				client := New(config.Config{LLMEnabled: true, LLMBaseURL: server.URL, LLMTimeout: time.Second})
				answer, err := client.Ask(context.Background(), Request{ChatID: 7, Text: "Объясни"})
				if err == nil || strings.Contains(err.Error(), "PRIVATE") || answer != "" || len(client.history[7]) != 0 {
					t.Fatalf("answer=%q err=%v history=%#v", answer, err, client.history[7])
				}
			})
		}
	}
}

func TestLengthRetryIsCappedAndHonorsConfiguredLimit(t *testing.T) {
	for _, test := range []struct {
		configured int
		limits     []int
	}{
		{2048, []int{2048, 8192}},
		{20000, []int{20000, 32768}},
		{65536, []int{65536}},
	} {
		t.Run(fmt.Sprint(test.configured), func(t *testing.T) {
			var limits []int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				limits = append(limits, payload.MaxTokens)
				fmt.Fprint(w, `{"choices":[{"finish_reason":"length","message":{"content":""}}]}`)
			}))
			defer server.Close()
			client := New(config.Config{LLMBaseURL: server.URL, LLMMaxTokens: test.configured, LLMTimeout: time.Second})
			_, err := client.complete(context.Background(), nil, nil, false)
			if !errors.Is(err, errAnswerLimit) || !reflect.DeepEqual(limits, test.limits) {
				t.Fatalf("err=%v limits=%v, want %v", err, limits, test.limits)
			}
		})
	}
}
