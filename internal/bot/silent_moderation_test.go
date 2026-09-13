package bot

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

type moderationTransport func(*http.Request) (*http.Response, error)

func (f moderationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSilentModerationPersistenceAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// Existing configuration must remain compatible.
	if err := os.WriteFile(path, []byte(`{"enabled_chats":[-2]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableSilent(-1); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableSilent(-1); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(-1, false); err == nil {
		t.Fatal("disabled locked moderation")
	}
	s, err = loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Silent(-1) || !s.Enabled(-1) || s.Silent(-2) || !s.Enabled(-2) {
		t.Fatal("incorrect restored settings")
	}
	if err := s.Set(-2, false); err != nil {
		t.Fatal(err)
	}
}

func TestSilentModerationSaveFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := loadLinkDeletionSettings(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "settings.json")
	if err := s.EnableSilent(-1); err == nil {
		t.Fatal("expected save failure")
	}
	if s.Silent(-1) || s.Enabled(-1) {
		t.Fatal("failed save changed settings")
	}
}

func TestSilentModerationUpdates(t *testing.T) {
	ctx := context.Background()
	s, err := loadLinkDeletionSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := loadAllowlist(filepath.Join(t.TempDir(), "allowed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := allowed.Add("allowed.example"); err != nil {
		t.Fatal(err)
	}
	b := &Bot{linkConfig: s, allowlist: allowed, telegram: telegram.New("test"), state: NewState(), adminCache: map[adminKey]adminEntry{
		{ChatID: -1, UserID: 1}: {admin: true, expires: time.Now().Add(time.Hour)},
		{ChatID: -1, UserID: 2}: {admin: false, expires: time.Now().Add(time.Hour)},
	}}
	deletions := 0
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/deleteMessage") {
			t.Errorf("unexpected Telegram request: %s", r.URL.Path)
		} else {
			deletions++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	message := func(text string, author int64) *telegram.Message {
		return &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: author}, Text: text}
	}
	for _, author := range []int64{2, 1} {
		if err := b.HandleUpdate(ctx, telegram.Update{Message: message("blahaj", author)}); err != nil {
			t.Fatal(err)
		}
		if s.Silent(-1) != (author == 1) {
			t.Fatal("incorrect activation permissions")
		}
	}
	for _, text := range []string{"blahaj", "/linkdelete off", "/ping", "/reset", "hello", "https://youtube.com/watch?v=abc", "https://allowed.example/page"} {
		if err := b.HandleUpdate(ctx, telegram.Update{Message: message(text, 2)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, edited := range []bool{false, true} {
		m := message("/ping https://blocked.example", 2)
		update := telegram.Update{Message: m}
		if edited {
			update = telegram.Update{EditedMessage: m}
		}
		if err := b.HandleUpdate(ctx, update); err != nil {
			t.Fatal(err)
		}
	}
	m := message("", 2)
	m.Caption = "hidden link"
	m.CaptionEntities = []telegram.MessageEntity{{Type: "text_link", URL: "https://blocked.example", Length: 6}}
	if err := b.HandleUpdate(ctx, telegram.Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message("https://blocked.example", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleUpdate(ctx, telegram.Update{CallbackQuery: &telegram.CallbackQuery{Message: message("", 1), Data: "donate:10"}}); err != nil {
		t.Fatal(err)
	}
	store, err := reminders.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b.reminders = store
	reminder := reminders.Reminder{ChatID: -1, Text: "test", TriggerAt: time.Now().Add(-time.Minute)}
	reminder.ID, err = store.Save(ctx, reminder)
	if err != nil {
		t.Fatal(err)
	}
	b.deliverReminder(ctx, reminder)
	due, err := store.GetDue(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatal("silent reminder was not completed")
	}
	if deletions != 5 {
		t.Fatalf("got %d deletions, want 5", deletions)
	}
	if !s.Enabled(-1) || !s.Silent(-1) {
		t.Fatal("mode was disabled")
	}
	// Private chats and unrelated groups still respond to commands.
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected private request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)), Header: make(http.Header)}, nil
	})
	for _, chat := range []telegram.Chat{{ID: 1, Type: "private"}, {ID: -2, Type: "group"}} {
		other := message("/ping", 1)
		other.Chat = chat
		if err := b.HandleUpdate(ctx, telegram.Update{Message: other}); err != nil {
			t.Fatal(err)
		}
	}
}
