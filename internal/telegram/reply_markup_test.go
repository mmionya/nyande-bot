package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostTranslationReplyMarkupSurvivesTelegramRoundTrip(t *testing.T) {
	var update Update
	err := json.Unmarshal([]byte(`{"message":{"message_id":12,"chat":{"id":42},"text":"/translate en","reply_to_message":{"message_id":11,"reply_markup":{"inline_keyboard":[[{"text":"Перевести","callback_data":"twtranslate:123"}]]}}}}`), &update)
	if err != nil || update.Message == nil || update.Message.ReplyToMessage == nil || update.Message.ReplyToMessage.ReplyMarkup == nil {
		t.Fatalf("reply keyboard was lost: %+v %v", update, err)
	}
	markup := update.Message.ReplyToMessage.ReplyMarkup
	if got := markup.InlineKeyboard[0][0].CallbackData; got != "twtranslate:123" {
		t.Fatalf("post identity=%q", got)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/editMessageReplyMarkup" {
			t.Errorf("wrong method: %s", r.URL)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if string(payload["chat_id"]) != "42" || string(payload["message_id"]) != "11" || len(payload) != 3 {
			t.Errorf("edit must only change original message keyboard: %s", payload)
		}
		var got InlineKeyboardMarkup
		if err := json.Unmarshal(payload["reply_markup"], &got); err != nil || len(got.InlineKeyboard) != 1 || got.InlineKeyboard[0][0].CallbackData != "twtranslate:123" {
			t.Errorf("keyboard changed: %+v %v", got, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":11}}`))
	}))
	defer server.Close()
	c := New("test")
	c.apiBaseURL = server.URL + "/"
	if err := c.EditMessageReplyMarkup(context.Background(), 42, 11, markup); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("edits=%d", calls)
	}
}
