package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestEditedMessagesEnforceLinkDeletionWithoutDownloads(t *testing.T) {
	settings, err := loadLinkDeletionSettings(filepath.Join(t.TempDir(), "moderation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(-1, true); err != nil {
		t.Fatal(err)
	}
	allowed, err := loadAllowlist(filepath.Join(t.TempDir(), "allowed.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{linkConfig: settings, allowlist: allowed, telegram: telegram.New("test"), adminCache: map[adminKey]adminEntry{
		{ChatID: -1, UserID: 1}: {admin: true, expires: time.Now().Add(time.Hour)},
		{ChatID: -1, UserID: 2}: {admin: false, expires: time.Now().Add(time.Hour)},
	}}
	var deleted []int
	requests := 0
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		var payload struct {
			MessageID int `json:"message_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch filepath.Base(r.URL.Path) {
		case "deleteMessage":
			deleted = append(deleted, payload.MessageID)
		case "sendMessage":
		default:
			t.Errorf("edited message triggered unexpected request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	message := &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: 2}, Text: "https://blocked.example"}
	if err := b.HandleUpdate(context.Background(), telegram.Update{EditedMessage: message}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != 10 {
		t.Fatalf("edited forbidden link escaped moderation: deleted=%v", deleted)
	}
	before := requests
	// Edited media links must not invoke the downloader, and admin edits remain exempt.
	for _, author := range []int64{1, 2} {
		message.From.ID = author
		for _, text := range []string{"https://youtube.com/watch?v=abc", "ordinary edited text"} {
			message.Text = text
			if err := b.HandleUpdate(context.Background(), telegram.Update{EditedMessage: message}); err != nil {
				t.Fatal(err)
			}
		}
	}
	message.From.ID, message.Text = 1, "https://blocked.example"
	if err := b.HandleUpdate(context.Background(), telegram.Update{EditedMessage: message}); err != nil {
		t.Fatal(err)
	}
	if requests != before {
		t.Fatal("non-moderated edit caused a response or download")
	}
}

func TestLinkDeletionSettingsPersistPerChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "link-moderation.json")
	settings, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled(-1001) {
		t.Fatal("link deletion should be disabled by default")
	}
	if err := settings.Set(-1001, true); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(-1002, true); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Enabled(-1001) || !reloaded.Enabled(-1002) {
		t.Fatal("enabled chats were not restored")
	}
	if reloaded.Enabled(-1003) {
		t.Fatal("an unrelated chat should keep the default setting")
	}

	if err := reloaded.Set(-1001, false); err != nil {
		t.Fatal(err)
	}
	restarted, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Enabled(-1001) || !restarted.Enabled(-1002) {
		t.Fatal("updated per-chat settings were not persisted")
	}
}
