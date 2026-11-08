package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSendMessageWithEntitiesPreservesTextAndReply(t *testing.T) {
	client := New("test-token")
	text := "😺\n  if (a < b && b > 0) { x **= 2; }\n"
	entities := []MessageEntity{{Type: "pre", Offset: 3, Length: len(utf16.Encode([]rune(text))) - 3, Language: "js"}}
	client.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Text      string          `json:"text"`
			Entities  []MessageEntity `json:"entities"`
			ParseMode string          `json:"parse_mode"`
			Reply     struct {
				MessageID int `json:"message_id"`
			} `json:"reply_parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Text != text || !reflect.DeepEqual(payload.Entities, entities) || payload.ParseMode != "" || payload.Reply.MessageID != 17 {
			t.Fatalf("formatted message changed: %+v", payload)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`))}, nil
	})
	message, err := client.SendMessageWithEntities(context.Background(), 1, text, entities, 17, nil)
	if err != nil || message.MessageID != 99 {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestSendMessageWithEntitiesOnlyFallsBackAfterEntityRejection(t *testing.T) {
	for _, failure := range []string{"entities", "other bad request", "transport", "read", "invalid JSON", "server error"} {
		t.Run(failure, func(t *testing.T) {
			client := New("test-token")
			calls := 0
			client.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					Text     string          `json:"text"`
					Entities []MessageEntity `json:"entities"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.Text != "important" || (len(payload.Entities) > 0) != (calls == 1) {
					t.Fatalf("invalid fallback payload: %+v (call %d)", payload, calls)
				}
				if calls > 1 {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`))}, nil
				}
				status, body := 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`
				switch failure {
				case "other bad request":
					body = `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
				case "transport":
					return nil, io.ErrUnexpectedEOF
				case "read":
					return &http.Response{StatusCode: 200, Body: io.NopCloser(brokenResponse{})}, nil
				case "invalid JSON":
					status, body = 502, "bad gateway"
				case "server error":
					status, body = 500, `{"ok":false,"error_code":500,"description":"Internal error"}`
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			_, err := client.SendMessageWithEntities(context.Background(), 1, "important", []MessageEntity{{Type: "bold", Length: 9}}, 17, nil)
			if failure == "entities" {
				if err != nil || calls != 2 {
					t.Fatalf("explicit rejection did not fall back: calls=%d err=%v", calls, err)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("unconfirmed or unrelated failure was retried: calls=%d err=%v", calls, err)
			}
		})
	}
}
