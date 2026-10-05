package quotes

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuotesPersistAndStayInTheirChat(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quotes.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.Add(ctx, Quote{ChatID: -100, MessageID: 4, AuthorID: 7, SavedBy: 8, Author: "Маша", Text: "Это не баг, это кот", Date: 1700000000})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.Add(ctx, Quote{ChatID: -100, MessageID: 4, AuthorID: 9, SavedBy: 9, Text: "Подмена"})
	if err != nil || duplicate != q {
		t.Fatalf("duplicate changed original: %#v, %v", duplicate, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Get(ctx, q.ChatID, q.ID); err != nil || got != q {
		t.Fatalf("reopen: %#v, %v", got, err)
	}
	if _, err := s.Get(ctx, -200, q.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-chat get: %v", err)
	}
	if _, err := s.Random(ctx, -200); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-chat random: %v", err)
	}
	if got, err := s.List(ctx, -200); err != nil || len(got) != 0 {
		t.Fatalf("cross-chat list: %#v, %v", got, err)
	}
	if got, err := s.Random(ctx, q.ChatID); err != nil || got != q {
		t.Fatalf("random: %#v, %v", got, err)
	}
	if deleted, err := s.Delete(ctx, -200, q.ID, 8, true); err != nil || deleted {
		t.Fatalf("cross-chat delete: %v, %v", deleted, err)
	}
	if deleted, err := s.Delete(ctx, q.ChatID, q.ID, 9, false); err != nil || deleted {
		t.Fatalf("unauthorized delete: %v, %v", deleted, err)
	}
	if deleted, err := s.Delete(ctx, q.ChatID, q.ID, 7, false); err != nil || !deleted {
		t.Fatalf("author delete: %v, %v", deleted, err)
	}
	if _, err := s.Random(ctx, q.ChatID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted quote still present: %v", err)
	}
}

func TestQuoteLimitsAndDeletePermissions(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, content := range []string{"", "  ", strings.Repeat("я", MaxTextRunes+1)} {
		if _, err := s.Add(ctx, Quote{Text: content}); err == nil {
			t.Fatal("invalid quote accepted")
		}
	}
	for _, permission := range []struct {
		user  int64
		admin bool
	}{{8, false}, {9, true}} {
		q, err := s.Add(ctx, Quote{ChatID: 1, MessageID: int(permission.user), AuthorID: 7, SavedBy: 8, Text: strings.Repeat("я", MaxTextRunes)})
		if err != nil {
			t.Fatal(err)
		}
		if deleted, err := s.Delete(ctx, 1, q.ID, permission.user, permission.admin); err != nil || !deleted {
			t.Fatalf("delete: %v %v", deleted, err)
		}
	}
}

func TestQuoteTriggerPersistsWithoutChangingQuotes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quotes.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	q, err := s.Add(ctx, Quote{ChatID: -1, MessageID: 4, AuthorID: 7, SavedBy: 8, Author: "Автор", Text: "Старая цитата"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the previous schema, which contained quotes but no trigger settings.
	if _, err := s.db.Exec("DROP TABLE quote_settings"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Trigger(ctx, -1); err != nil || got != "" {
		t.Fatalf("new setting: %q, %v", got, err)
	}
	for _, trigger := range []string{strings.Repeat("я", 32), "  Цитата_2-test  "} {
		if err := s.SetTrigger(ctx, -1, trigger); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTrigger(ctx, -2, "другое"); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{"two words", "quote!", "/quote", "bad\nword", "\xff", strings.Repeat("я", 33)} {
		if err := s.SetTrigger(ctx, -1, trigger); !errors.Is(err, ErrInvalidTrigger) {
			t.Fatalf("accepted %q: %v", trigger, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for chat, want := range map[int64]string{-1: "цитата_2-test", -2: "другое", -3: ""} {
		if got, err := s.Trigger(ctx, chat); err != nil || got != want {
			t.Fatalf("chat %d: %q, %v", chat, got, err)
		}
	}
	if err := s.SetTrigger(ctx, -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for chat, want := range map[int64]string{-1: "", -2: "другое"} {
		if got, err := s.Trigger(ctx, chat); err != nil || got != want {
			t.Fatalf("disabled chat %d: %q, %v", chat, got, err)
		}
	}
	if got, err := s.Get(ctx, -1, q.ID); err != nil || got != q {
		t.Fatalf("settings changed an old quote: %#v, %v", got, err)
	}
}
