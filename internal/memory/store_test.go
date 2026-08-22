package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T, maximum int) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"), maximum)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRememberRecallAndScope(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, 10)
	first, created, err := store.Remember(ctx, 1, 7, "Илья", "  Любит   зелёный чай  ")
	if err != nil || !created || first.Content != "Любит зелёный чай" {
		t.Fatalf("unexpected first memory: %#v created=%t err=%v", first, created, err)
	}
	repeated, created, err := store.Remember(ctx, 1, 7, "Илья", "Любит зелёный чай")
	if err != nil || created || repeated.ID != first.ID {
		t.Fatalf("duplicate was not refreshed: %#v created=%t err=%v", repeated, created, err)
	}
	_, _, _ = store.Remember(ctx, 1, 8, "Катя", "Любит кофе")
	_, _, _ = store.Remember(ctx, 2, 7, "Илья", "Любит какао")

	memories, err := store.Recall(ctx, 1, 7, "какой чай я люблю?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].ID != first.ID {
		t.Fatalf("scope or FTS recall failed: %#v", memories)
	}
}

func TestMaximumForgetAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"first fact", "second fact", "third fact"} {
		if _, _, err := store.Remember(ctx, 3, 9, "User", content); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List(ctx, 3, 9, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("maximum was not applied: %#v err=%v", items, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	items, err = reopened.List(ctx, 3, 9, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("memories did not persist: %#v err=%v", items, err)
	}
	if deleted, err := reopened.Forget(ctx, 3, 8, items[0].ID); err != nil || deleted {
		t.Fatalf("another user deleted memory: deleted=%t err=%v", deleted, err)
	}
	if deleted, err := reopened.Forget(ctx, 3, 9, items[0].ID); err != nil || !deleted {
		t.Fatalf("memory was not deleted: deleted=%t err=%v", deleted, err)
	}
	if count, err := reopened.ForgetAll(ctx, 3, 9); err != nil || count != 1 {
		t.Fatalf("forget all removed %d, err=%v", count, err)
	}
}

func TestSensitiveAndOversizedMemoriesAreRejected(t *testing.T) {
	store := openTestStore(t, 10)
	ctx := context.Background()
	for _, test := range []struct {
		content string
		want    error
	}{
		{"", ErrEmpty},
		{strings.Repeat("я", MaxContentRunes+1), ErrTooLong},
		{"мой пароль: hunter2", ErrSensitive},
		{"API key = sk-1234567890abcdefghijkl", ErrSensitive},
	} {
		if _, _, err := store.Remember(ctx, 1, 1, "", test.content); !errors.Is(err, test.want) {
			t.Errorf("Remember(%q) error=%v, want %v", test.content, err, test.want)
		}
	}
}
