package bot

import (
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/quotes"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestQuoteTriggerThroughHandleUpdate(t *testing.T) {
	store, err := quotes.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := &Bot{quotes: store, telegram: telegram.New("test"), state: NewState(), llm: llm.New(config.Config{}), adminCache: map[adminKey]adminEntry{
		{ChatID: -1, UserID: 1}: {admin: true, expires: time.Now().Add(time.Hour)},
		{ChatID: -1, UserID: 2}: {admin: false, expires: time.Now().Add(time.Hour)},
	}}
	photos := 0
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true,"result":{"message_id":99}}`
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
		case strings.HasSuffix(r.URL.Path, "/getUserProfilePhotos"):
			var request struct {
				UserID int64 `json:"user_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.UserID != 7 {
				t.Fatalf("avatar must belong to parent author: %#v, %v", request, err)
			}
			body = `{"ok":true,"result":{"total_count":0,"photos":[]}}`
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			photos++
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Fatal(err)
			}
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("photo")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := png.DecodeConfig(file); err != nil {
				t.Fatal(err)
			}
			var reply struct {
				MessageID int `json:"message_id"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != 10 || r.FormValue("chat_id") != "-1" {
				t.Fatalf("wrong photo destination: %s %#v %v", r.FormValue("chat_id"), reply, err)
			}
		default:
			t.Fatalf("unexpected request (admin must be cached/private): %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	for _, test := range []struct {
		text    string
		user    int64
		private bool
		want    string
	}{
		{"/quote trigger", 2, false, ""},
		{"/quote trigger цитата", 2, false, ""},
		{"/quote trigger Цитата", 1, false, "цитата"},
		{"/quote trigger off", 2, false, "цитата"},
		{"/quote trigger bad!", 1, false, "цитата"},
		{"/quote trigger " + silentModerationTrigger, 1, false, "цитата"},
		{"/quote trigger два слова", 1, false, "цитата"},
		{"/quote trigger личное", 2, true, "личное"},
	} {
		m := permissionMessage(10, test.user, test.text)
		if test.private {
			m.Chat = telegram.Chat{ID: 2, Type: "private"}
		}
		permissionUpdate(t, b, m, false)
		if got, err := store.Trigger(context.Background(), m.Chat.ID); err != nil || got != test.want {
			t.Fatalf("configuration %q user=%d: got %q, %v", test.text, test.user, got, err)
		}
	}
	parent := &telegram.Message{MessageID: 41, From: &telegram.User{ID: 7, FirstName: "Автор"}, Chat: telegram.Chat{ID: -1}, Caption: "Подпись родительского сообщения"}
	for _, test := range []struct {
		text                                      string
		noReply, bot, otherChat, senderChat, want bool
	}{
		{text: " \tЦиТаТа\n", want: true},
		{text: "цитата", want: true}, // Duplicate saves reuse the quote, but still render a requested card.
		{text: "цитата пожалуйста"}, {text: "нецитата"}, {text: "цитата!"},
		{text: "цитата", noReply: true}, {text: "/цитата"},
		{text: "цитата", bot: true}, {text: "цитата", otherChat: true}, {text: "цитата", senderChat: true},
	} {
		m := permissionMessage(10, 2, test.text)
		m.ReplyToMessage, m.From.IsBot = parent, test.bot
		if test.noReply {
			m.ReplyToMessage = nil
		}
		if test.otherChat {
			m.Chat.ID = -2
		}
		if test.senderChat {
			m.SenderChat = &telegram.Chat{ID: -1}
		}
		before := photos
		permissionUpdate(t, b, m, false)
		if (photos == before+1) != test.want || photos > before+1 {
			t.Fatalf("trigger %#v sent %d cards", test, photos-before)
		}
	}
	items, err := store.List(context.Background(), -1)
	if err != nil || len(items) != 1 || items[0].MessageID != 41 || items[0].AuthorID != 7 || items[0].Author != "Автор" || items[0].SavedBy != 2 || items[0].Text != parent.Caption {
		t.Fatalf("trigger quoted the wrong message: %#v, %v", items, err)
	}
	permissionUpdate(t, b, permissionMessage(10, 1, "/quote trigger off"), false)
	m := permissionMessage(10, 2, "цитата")
	m.ReplyToMessage = parent
	before := photos
	permissionUpdate(t, b, m, false)
	if photos != before {
		t.Fatal("disabled trigger sent a card")
	}
	m.Text = "/quote"
	permissionUpdate(t, b, m, false)
	if photos != before+1 {
		t.Fatal("disabling trigger disabled /quote")
	}
}
