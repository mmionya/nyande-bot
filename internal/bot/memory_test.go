package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
	longmemory "github.com/mmionya/nyande-bot/internal/memory"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestCommandsForAnotherBotCannotDeleteMemories(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(filepath.Join(t.TempDir(), "memory.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := &Bot{memories: store, telegram: telegram.New("test"), state: NewState(), identity: telegram.User{Username: "nyande_bot"}}
	requests := 0
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, chat := range []telegram.Chat{{ID: 7, Type: "private"}, {ID: -1, Type: "supergroup"}} {
		if _, _, err := store.Remember(ctx, chat.ID, 7, "Илья", "Предпочитает короткие ответы"); err != nil {
			t.Fatal(err)
		}
		message := &telegram.Message{Chat: chat, From: &telegram.User{ID: 7}}
		before := requests
		for _, text := range []string{"/forget_all@other_bot", "/ping@other_bot мяу https://example.com"} {
			message.Text = text
			if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
		}
		if items, err := store.List(ctx, chat.ID, 7, 10); err != nil || len(items) != 1 {
			t.Fatalf("another bot's command deleted memory: items=%v err=%v", items, err)
		}
		if requests != before {
			t.Fatal("another bot's command caused a Telegram request")
		}
		message.Text = "/forget_all@NYANDE_BOT"
		if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
			t.Fatal(err)
		}
		if items, err := store.List(ctx, chat.ID, 7, 10); err != nil || len(items) != 0 || requests != before+1 {
			t.Fatalf("own command did not delete memory and confirm it: items=%v requests=%d err=%v", items, requests-before, err)
		}
	}
}

func TestMemoryToolsRememberRecallAndForget(t *testing.T) {
	store, err := longmemory.Open(filepath.Join(t.TempDir(), "memory.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	application := &Bot{
		cfg: config.Config{LLMMemoryRecall: 5}, memories: store,
	}
	message := &telegram.Message{
		Chat: telegram.Chat{ID: 100, Type: "private"},
		From: &telegram.User{ID: 7, FirstName: "Илья"},
	}
	tools := application.memoryTools(message)
	if len(tools) != 2 {
		t.Fatalf("got %d memory tools, want 2", len(tools))
	}
	observation, err := tools[0].Execute(context.Background(), map[string]string{
		"fact": "Предпочитает короткие ответы",
	})
	if err != nil || !strings.Contains(observation, "Saved long-term memory") {
		t.Fatalf("remember tool result=%q err=%v", observation, err)
	}
	items := application.recallMemories(context.Background(), message, "ответы")
	if len(items) != 1 || items[0].Content != "Предпочитает короткие ответы" {
		t.Fatalf("unexpected recalled memories: %#v", items)
	}
	observation, err = tools[1].Execute(context.Background(), map[string]string{
		"memory_id": " 1\n",
	})
	if err != nil || !strings.Contains(observation, "Deleted long-term memory") {
		t.Fatalf("forget tool result=%q err=%v", observation, err)
	}
	if items := application.recallMemories(context.Background(), message, "ответы"); len(items) != 0 {
		t.Fatalf("memory survived deletion: %#v", items)
	}
}

func TestLLMSavesUserFactWithoutRememberCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := longmemory.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	application := &Bot{cfg: config.Config{LLMMemoryRecall: 5}, memories: store}
	message := &telegram.Message{Chat: telegram.Chat{ID: 100, Type: "private"}, From: &telegram.User{ID: 7, FirstName: "Илья"}}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var payload struct {
			Messages []struct{ Role, Content string }
			Tools    []struct {
				Function struct{ Name, Description string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if len(payload.Messages) < 2 {
			t.Error("missing system and user messages")
			return
		}
		last := payload.Messages[len(payload.Messages)-1]
		if requests == 1 {
			system := payload.Messages[0]
			if system.Role != "system" || !strings.Contains(system.Content, "Отвечай кратко.") || !strings.Contains(system.Content, "without an explicit request") {
				t.Error("custom system prompt lost the proactive memory instruction")
			}
			advertised := false
			for _, tool := range payload.Tools {
				advertised = advertised || tool.Function.Name == "remember_memory" && strings.Contains(tool.Function.Description, "without an explicit request")
			}
			if !advertised || last.Role != "user" || last.Content != "Илья: Я живу в Алматы." {
				t.Error("memory tool or ordinary user statement missing")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"memory-1","type":"function","function":{"name":"remember_memory","arguments":"{\"fact\":\"Живёт в Алматы\"}"}}]}}]}`)
			return
		}
		if last.Role != "tool" || !strings.Contains(last.Content, "Saved long-term memory") {
			t.Errorf("missing memory observation: %#v", last)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Понятно."}}]}`)
	}))
	defer server.Close()
	client := llm.New(config.Config{LLMEnabled: true, LLMBaseURL: server.URL, LLMModel: "test", LLMTimeout: time.Second, LLMSystemPrompt: "Отвечай кратко."})
	answer, err := client.Ask(context.Background(), llm.Request{ChatID: 100, UserName: "Илья", Text: "Я живу в Алматы.", Tools: application.memoryTools(message)})
	if err != nil || answer != "Понятно." || requests != 2 {
		t.Fatalf("answer=%q err=%v requests=%d", answer, err, requests)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := longmemory.Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	application.memories = reopened
	if items := application.recallMemories(context.Background(), message, "Алматы"); len(items) != 1 || items[0].Content != "Живёт в Алматы" {
		t.Fatalf("fact did not survive reopening: %#v", items)
	}
	message.From = &telegram.User{ID: 8, FirstName: "Другой"}
	if items := application.recallMemories(context.Background(), message, "Алматы"); len(items) != 0 {
		t.Fatalf("another user can recall the fact: %#v", items)
	}
}
