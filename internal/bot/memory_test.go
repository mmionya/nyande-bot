package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyphentae/nyande-bot/internal/config"
	longmemory "github.com/hyphentae/nyande-bot/internal/memory"
	"github.com/hyphentae/nyande-bot/internal/telegram"
)

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
		"memory_id": "1",
	})
	if err != nil || !strings.Contains(observation, "Deleted long-term memory") {
		t.Fatalf("forget tool result=%q err=%v", observation, err)
	}
	if items := application.recallMemories(context.Background(), message, "ответы"); len(items) != 0 {
		t.Fatalf("memory survived deletion: %#v", items)
	}
}
