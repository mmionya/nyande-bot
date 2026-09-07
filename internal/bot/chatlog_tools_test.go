package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/chatlog"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestChatlogToolsNilWhenNoStore(t *testing.T) {
	b := &Bot{}
	msg := &telegram.Message{Chat: telegram.Chat{ID: 123}}
	tools := b.chatlogTools(msg)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools when chatlog is nil, got %d", len(tools))
	}
}

func TestChatlogToolsExecution(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-tools.db")
	store, err := chatlog.Open(path, 100)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	b := &Bot{chatlog: store}
	msg := &telegram.Message{
		Chat: telegram.Chat{ID: -1001},
		From: &telegram.User{ID: 10, FirstName: "Иван", Username: "ivan"},
	}

	tools := b.chatlogTools(msg)
	if len(tools) != 2 {
		t.Fatalf("expected 2 chatlog tools, got %d", len(tools))
	}

	searchTool := tools[0]
	recentTool := tools[1]

	if searchTool.Name != "search_chat_messages" {
		t.Errorf("expected tool name search_chat_messages, got %s", searchTool.Name)
	}
	if recentTool.Name != "get_recent_messages" {
		t.Errorf("expected tool name get_recent_messages, got %s", recentTool.Name)
	}

	// Test search on empty store
	res, err := searchTool.Execute(ctx, map[string]string{"query": "шашлыки"})
	if err != nil {
		t.Fatalf("search execute error: %v", err)
	}
	if !strings.Contains(res, "No messages found") {
		t.Errorf("expected 'No messages found', got: %s", res)
	}

	// Test search with empty query
	resEmpty, _ := searchTool.Execute(ctx, map[string]string{"query": "  "})
	if !strings.Contains(resEmpty, "non-empty") {
		t.Errorf("expected non-empty query error, got: %s", resEmpty)
	}

	// Add messages to store
	b.saveChatMessage(ctx, &telegram.Message{
		Chat:      telegram.Chat{ID: -1001},
		MessageID: 1,
		From:      &telegram.User{ID: 10, FirstName: "Иван", Username: "ivan"},
		Text:      "Пойдем завтра в кино на премьеру?",
		Date:      time.Now().Unix(),
	})
	b.saveChatMessage(ctx, &telegram.Message{
		Chat:      telegram.Chat{ID: -1001},
		MessageID: 2,
		From:      &telegram.User{ID: 20, FirstName: "Анна"},
		Text:      "Да, отличное кино, я за!",
		Date:      time.Now().Add(1 * time.Minute).Unix(),
	})

	// Search for "кино"
	resSearch, err := searchTool.Execute(ctx, map[string]string{"query": "кино"})
	if err != nil {
		t.Fatalf("search execute error: %v", err)
	}
	if !strings.Contains(resSearch, "Found 2 messages") || !strings.Contains(resSearch, "Иван") || !strings.Contains(resSearch, "Анна") {
		t.Errorf("unexpected search output: %s", resSearch)
	}

	// Test recent tool
	resRecent, err := recentTool.Execute(ctx, map[string]string{"limit": "5"})
	if err != nil {
		t.Fatalf("recent execute error: %v", err)
	}
	if !strings.Contains(resRecent, "Recent 2 messages") || !strings.Contains(resRecent, "премьеру") {
		t.Errorf("unexpected recent output: %s", resRecent)
	}
}

func TestSaveChatMessageIgnoresEmpty(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-empty.db")
	store, err := chatlog.Open(path, 100)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	b := &Bot{chatlog: store}

	// Empty text should not be saved
	b.saveChatMessage(ctx, &telegram.Message{
		Chat:      telegram.Chat{ID: 100},
		MessageID: 1,
		Text:      "   ",
	})
	msgs, err := store.Recent(ctx, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages saved, got %d", len(msgs))
	}
}
