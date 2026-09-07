package reminders

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRemindersStoreSaveAndGetDue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-reminders.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()

	// Due reminder
	id1, err := store.Save(ctx, Reminder{
		ChatID:    100,
		UserID:    200,
		UserName:  "testuser",
		Text:      "Купить молоко",
		TriggerAt: now.Add(-10 * time.Second),
		CreatedAt: now.Add(-1 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Save 1 failed: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("expected positive ID, got %d", id1)
	}

	// Future reminder (not due yet)
	_, err = store.Save(ctx, Reminder{
		ChatID:    100,
		UserID:    200,
		UserName:  "testuser",
		Text:      "Позвонить врачу",
		TriggerAt: now.Add(1 * time.Hour),
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("Save 2 failed: %v", err)
	}

	due, err := store.GetDue(ctx, now)
	if err != nil {
		t.Fatalf("GetDue failed: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due reminder, got %d", len(due))
	}
	if due[0].ID != id1 || due[0].Text != "Купить молоко" {
		t.Fatalf("unexpected due reminder: %+v", due[0])
	}

	// Mark completed
	if err := store.MarkCompleted(ctx, id1); err != nil {
		t.Fatalf("MarkCompleted failed: %v", err)
	}

	dueAfter, err := store.GetDue(ctx, now)
	if err != nil {
		t.Fatalf("GetDue after complete failed: %v", err)
	}
	if len(dueAfter) != 0 {
		t.Fatalf("expected 0 due reminders after mark, got %d", len(dueAfter))
	}
}
