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

	"github.com/mmionya/nyande-bot/internal/access"
	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestBannedTelegramUserCannotUseBot(t *testing.T) {
	store, err := access.Open(filepath.Join(t.TempDir(), "bans.json"), []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetBanned("2", true); err != nil {
		t.Fatal(err)
	}
	// No service dependencies: any accidental dispatch to downloads/LLM/games fails.
	b := &Bot{access: store, telegram: telegram.New("test")}
	ctx := context.Background()
	for _, chat := range []telegram.Chat{{ID: 2, Type: "private"}, {ID: -1, Type: "group"}, {ID: -2, Type: "supergroup"}} {
		for _, text := range []string{"/start", "/ping", "/botunban 2", "/quote", "/gif", "/donate", "мяу привет", "apple", "https://youtube.com/watch?v=abc"} {
			message := &telegram.Message{Chat: chat, From: &telegram.User{ID: 2}, Text: text, Voice: &telegram.Voice{FileID: "voice"}}
			for _, update := range []telegram.Update{{Message: message}, {EditedMessage: message}} {
				if err := b.HandleUpdate(ctx, update); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, automatic := range []bool{false, true} {
		// Channel identities cannot be used to evade a user ban, even with a fake owner From.
		message := &telegram.Message{Chat: telegram.Chat{ID: -1}, From: &telegram.User{ID: 1}, SenderChat: &telegram.Chat{ID: -3}, IsAutomaticForward: automatic, Text: "https://youtube.com/watch?v=abc"}
		if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
			t.Fatal(err)
		}
	}
	requests := []string{}
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		method := filepath.Base(r.URL.Path)
		requests = append(requests, method)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if method == "answerPreCheckoutQuery" && body["ok"] != false {
			t.Fatal("approved payment from a banned user")
		}
		if method != "answerCallbackQuery" && method != "answerPreCheckoutQuery" {
			t.Errorf("banned user triggered %s", method)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, data := range []string{"donate:10", "ttt_accept:1", "chk_accept:1"} {
		query := &telegram.CallbackQuery{ID: "callback", From: telegram.User{ID: 2}, Data: data, Message: &telegram.Message{Chat: telegram.Chat{ID: -1}, From: &telegram.User{ID: 99}}}
		if err := b.HandleUpdate(ctx, telegram.Update{CallbackQuery: query}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.HandleUpdate(ctx, telegram.Update{PreCheckoutQuery: &telegram.PreCheckoutQuery{ID: "payment", From: telegram.User{ID: 2}, Currency: "XTR", TotalAmount: 10, InvoicePayload: "nyande_donation:10:2:nonce"}}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 {
		t.Fatalf("expected 4 rejected interactions, got %v", requests)
	}
	reminderStore, err := reminders.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer reminderStore.Close()
	b.reminders = reminderStore
	r := reminders.Reminder{UserID: 2, ChatID: -3, Text: "reminder", TriggerAt: time.Now().Add(-time.Minute)}
	r.ID, err = reminderStore.Save(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	b.deliverReminder(ctx, r)
	if due, err := reminderStore.GetDue(ctx, time.Now()); err != nil || len(due) != 0 {
		t.Fatalf("banned user's reminder was not discarded: %v, %v", due, err)
	}
}

func TestBotBanCommandsRequireOwnerAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	store, err := access.Open(path, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{access: store, telegram: telegram.New("test"), state: NewState(), identity: telegram.User{ID: 99, Username: "nyande_bot"}}
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`)), Header: make(http.Header)}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	ctx := context.Background()
	for _, tc := range []struct {
		text      string
		from      int64
		forwarded bool
	}{
		{"/botban 2", 3, false}, // A private-chat user is not an owner.
		{"/botban 2", 1, true},
		{"/botban@other_bot 2", 1, false},
		{"/botban @someone", 1, false},
		{"/botban -2", 1, false},
		{"/botban 1", 1, false},
		{"/botban 99", 1, false},
	} {
		message := &telegram.Message{Chat: telegram.Chat{ID: tc.from, Type: "private"}, From: &telegram.User{ID: tc.from}, Text: tc.text}
		if tc.forwarded {
			message.ForwardOrigin = &telegram.MessageOrigin{Type: "user"}
		}
		if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
			t.Fatal(err)
		}
		if store.IsBanned("2") || store.IsBanned("99") {
			t.Fatalf("invalid ban accepted: %q", tc.text)
		}
	}
	message := &telegram.Message{Chat: telegram.Chat{ID: -1, Type: "group"}, From: &telegram.User{ID: 1}, Text: "/botban@nyande_bot", ReplyToMessage: &telegram.Message{Chat: telegram.Chat{ID: -1}, From: &telegram.User{ID: 2}}}
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	store, err = access.Open(path, []string{"1"})
	if err != nil || !store.IsBanned("2") {
		t.Fatalf("ban not persisted: %v", err)
	}
	b.access = store
	message.Text, message.ReplyToMessage = "/botunban 2", nil
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	store, err = access.Open(path, []string{"1"})
	if err != nil || store.IsBanned("2") {
		t.Fatalf("unban not persisted: %v", err)
	}
	// Access resumes immediately after unban, including in another chat.
	message.Chat, message.From, message.Text = telegram.Chat{ID: 2, Type: "private"}, &telegram.User{ID: 2}, "/ping"
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
}

func TestBannedUserStillSubjectToLinkModeration(t *testing.T) {
	b, deleted := permissionTestBot(t)
	var err error
	b.access, err = access.Open(filepath.Join(t.TempDir(), "bans.json"), []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.access.SetBanned("2", true); err != nil {
		t.Fatal(err)
	}
	for _, edited := range []bool{false, true} {
		permissionUpdate(t, b, permissionMessage(10, 2, "https://youtube.com/watch?v=abc"), edited)
	}
	// Silent groups stay silent even when a banned user clicks an old button.
	if err := b.HandleUpdate(context.Background(), telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		ID: "old-button", From: telegram.User{ID: 2}, Message: permissionMessage(11, 99, ""), Data: "donate:10",
	}}); err != nil {
		t.Fatal(err)
	}
	if len(*deleted) != 2 {
		t.Fatalf("banned user's links escaped moderation: %v", *deleted)
	}
}
