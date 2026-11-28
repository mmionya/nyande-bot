package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func linkSecurityBot(t *testing.T, methods *[]string) *Bot {
	t.Helper()
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
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		*methods = append(*methods, filepath.Base(r.URL.Path))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":100}}`))}, nil
	})
	return &Bot{telegram: telegram.New("test"), state: NewState(), linkConfig: settings, allowlist: allowed,
		adminCache: map[adminKey]adminEntry{
			{ChatID: -1, UserID: 123}: {admin: false, expires: time.Now().Add(time.Hour)},
			{ChatID: -1, UserID: 1}:   {admin: true, expires: time.Now().Add(time.Hour)},
		}}
}

func TestAllowlinkOnlyAuthorizesItsChat(t *testing.T) {
	var methods []string
	b := linkSecurityBot(t, &methods)
	private := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 123, Type: "private"}, From: &telegram.User{ID: 123}, Text: "/allowlink blocked.example"}
	if err := b.HandleUpdate(context.Background(), telegram.Update{Message: private}); err != nil {
		t.Fatal(err)
	}
	if !b.allowlist.Allows(123, "https://blocked.example") || b.allowlist.Allows(-1, "https://blocked.example") {
		t.Fatal("private command changed another chat's permissions")
	}
	methods = nil
	group := &telegram.Message{MessageID: 2, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: private.From, Text: "https://blocked.example"}
	if err := b.HandleUpdate(context.Background(), telegram.Update{Message: group}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{"deleteMessage", "sendMessage"}) {
		t.Fatalf("private rule exempted group link: %v", methods)
	}
	group.Text = "/allowlink blocked.example"
	if err := b.HandleUpdate(context.Background(), telegram.Update{Message: group}); err != nil {
		t.Fatal(err)
	}
	if b.allowlist.Allows(-1, "https://blocked.example") {
		t.Fatal("non-admin changed group permissions")
	}
	group.From = &telegram.User{ID: 1}
	if err := b.HandleUpdate(context.Background(), telegram.Update{Message: group}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadAllowlist(b.allowlist.path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Allows(-1, "https://media.blocked.example/post") || reloaded.Allows(-2, "https://blocked.example") {
		t.Fatal("group rule did not persist with its chat scope")
	}
}

func TestLegacyAllowlistEntriesStayInactiveAndPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowed.json")
	if err := os.WriteFile(path, []byte(`{"domains":["legacy.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed, err := loadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Allows(-1, "https://legacy.example") {
		t.Fatal("legacy global permission remains active")
	}
	if _, added, err := allowed.Add(-1, "legacy.example"); err != nil || !added {
		t.Fatalf("cannot reauthorize legacy domain for a chat: added=%t err=%v", added, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved storedAllowlist
	if err := json.Unmarshal(data, &saved); err != nil || !reflect.DeepEqual(saved.Domains, []string{"legacy.example"}) {
		t.Fatalf("legacy data was lost: %s, %v", data, err)
	}
	reloaded, err := loadAllowlist(path)
	if err != nil || !reloaded.Allows(-1, "https://legacy.example") || reloaded.Allows(-2, "https://legacy.example") {
		t.Fatalf("migration broadened permissions: %v", err)
	}
}

func TestAllowlistFailedWriteDoesNotGrantPermission(t *testing.T) {
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := &allowlist{path: filepath.Join(blockedParent, "allowed.json")}
	if _, _, err := allowed.Add(-1, "blocked.example"); err == nil {
		t.Fatal("expected persistence failure")
	}
	if allowed.Allows(-1, "https://blocked.example") {
		t.Fatal("failed save published an in-memory permission")
	}
}

func TestCommandsCannotBypassLinkModeration(t *testing.T) {
	for _, command := range []string{"/unknown", "/ping", "/ping@other_bot", "/agentstatus", "/agentstop", "/translate", "/play"} {
		t.Run(command, func(t *testing.T) {
			var methods []string
			b := linkSecurityBot(t, &methods)
			// Busy fast paths must moderate links too, even without entering handleMessage.
			if command == "/translate" || command == "/play" {
				unlock := b.lockUpdates(-1, command == "/play")
				defer unlock()
			}
			message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: 123}, Text: command + " https://blocked.example"}
			if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(methods, []string{"deleteMessage", "sendMessage"}) {
				t.Fatalf("command escaped moderation or was dispatched: %v", methods)
			}
		})
	}
}

func TestModerationKeepsAdminAndChatAllowedCommands(t *testing.T) {
	for _, admin := range []bool{false, true} {
		var methods []string
		b := linkSecurityBot(t, &methods)
		author := int64(123)
		if admin {
			author = 1
		} else if _, _, err := b.allowlist.Add(-1, "allowed.example"); err != nil {
			t.Fatal(err)
		}
		message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: author}, Text: "/ping https://allowed.example"}
		if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(methods, []string{"sendMessage"}) {
			t.Fatalf("legitimate command was deleted: %v", methods)
		}
	}
}
