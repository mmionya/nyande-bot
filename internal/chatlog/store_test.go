package chatlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestChatLogStoreSaveAndSearch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-chatlog.db")
	store, err := Open(path, 100)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)

	msgs := []Message{
		{
			ChatID:      -1001,
			MessageID:   1,
			UserID:      42,
			Username:    "alice",
			DisplayName: "Алиса",
			Text:        "Привет всем, кто пойдет на шашлыки в субботу?",
			Date:        now.Add(-2 * time.Minute),
		},
		{
			ChatID:      -1001,
			MessageID:   2,
			UserID:      43,
			Username:    "bob",
			DisplayName: "Боб",
			Text:        "Я пойду, могу купить мясо и угли",
			Date:        now.Add(-1 * time.Minute),
		},
		{
			ChatID:      -1002, // Different chat
			MessageID:   3,
			UserID:      44,
			Username:    "charlie",
			DisplayName: "Чарли",
			Text:        "Шашлыки это круто, но я в другом чате",
			Date:        now,
		},
	}

	for _, m := range msgs {
		if err := store.Save(ctx, m); err != nil {
			t.Fatalf("Save failed for msg %d: %v", m.MessageID, err)
		}
	}

	// Search for "шашлыки" in chat -1001
	results, err := store.Search(ctx, -1001, "шашлыки", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].MessageID != 1 || results[0].DisplayName != "Алиса" {
		t.Fatalf("unexpected search result: %+v", results[0])
	}

	// Search in chat -1002
	results2, err := store.Search(ctx, -1002, "шашлыки", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results2) != 1 || results2[0].MessageID != 3 {
		t.Fatalf("expected msg 3 in chat -1002, got: %+v", results2)
	}

	// Search non-existent term
	resultsNone, err := store.Search(ctx, -1001, "пицца", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(resultsNone) != 0 {
		t.Fatalf("expected 0 results, got %d", len(resultsNone))
	}
}

func TestChatLogStoreRecent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-recent.db")
	store, err := Open(path, 100)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)

	for i := 1; i <= 5; i++ {
		err := store.Save(ctx, Message{
			ChatID:      -1001,
			MessageID:   i,
			UserID:      100,
			DisplayName: "User",
			Text:        time.Duration(i).String(),
			Date:        now.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("Save failed: %v", err)
		}
	}

	// Recent 3 should return messages 3, 4, 5 in chronological order (oldest to newest)
	recent, err := store.Recent(ctx, -1001, 3)
	if err != nil {
		t.Fatalf("Recent failed: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(recent))
	}
	if recent[0].MessageID != 3 || recent[1].MessageID != 4 || recent[2].MessageID != 5 {
		t.Fatalf("expected message IDs 3, 4, 5 in chronological order, got %d, %d, %d",
			recent[0].MessageID, recent[1].MessageID, recent[2].MessageID)
	}
}

func TestChatLogStoreUpdateExistingMessage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-update.db")
	store, err := Open(path, 100)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	msg := Message{
		ChatID:      -1001,
		MessageID:   10,
		UserID:      42,
		Username:    "alice",
		DisplayName: "Алиса",
		Text:        "Оригинальный текст",
		Date:        time.Now().UTC(),
	}
	if err := store.Save(ctx, msg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Update the message
	msg.Text = "Отредактированный текст с котиками"
	if err := store.Save(ctx, msg); err != nil {
		t.Fatalf("Save update failed: %v", err)
	}

	// Search for updated content
	results, err := store.Search(ctx, -1001, "котиками", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 1 || results[0].Text != "Отредактированный текст с котиками" {
		t.Fatalf("unexpected search result after update: %+v", results)
	}

	// Old content shouldn't match in FTS
	oldResults, err := store.Search(ctx, -1001, "Оригинальный", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(oldResults) != 0 {
		t.Fatalf("expected 0 results for old content, got %d", len(oldResults))
	}
}
