package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenResponse struct{}

func (brokenResponse) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestGetUpdatesIncludesEditedMessages(t *testing.T) {
	client := New("test-token")
	client.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		var request struct {
			AllowedUpdates []string `json:"allowed_updates"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(request.AllowedUpdates, "edited_message") {
			t.Fatal("message edits are not subscribed, so moderation and archive updates never receive them")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":8,"edited_message":{"message_id":3,"text":"edited","chat":{"id":-1}}}]}`))}, nil
	})
	updates, err := client.GetUpdates(context.Background(), 8)
	if err != nil || len(updates) != 1 || updates[0].EditedMessage == nil || updates[0].EditedMessage.Text != "edited" {
		t.Fatalf("edited message not received: updates=%+v err=%v", updates, err)
	}
}

func TestRetriesDoNotDuplicateUnconfirmedSends(t *testing.T) {
	for _, method := range []string{"sendDocument", "sendMediaGroup", "sendMessage", "getMe"} {
		for _, failure := range []string{"transport", "read", "invalid JSON", "server error", "rate limited"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				calls := 0
				client := New("test-token")
				client.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
					calls++
					status, body := http.StatusOK, `{"ok":true,"result":true}`
					if calls == 1 {
						switch failure {
						case "transport":
							return nil, io.ErrUnexpectedEOF
						case "read":
							return &http.Response{StatusCode: status, Body: io.NopCloser(brokenResponse{})}, nil
						case "invalid JSON":
							status, body = http.StatusBadGateway, "gateway error"
						case "server error":
							status, body = http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"Internal error"}`
						case "rate limited":
							status, body = http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too many requests"}`
						}
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				err := client.do(context.Background(), method, "application/json", []byte(`{}`), nil)
				if method == "getMe" || failure == "rate limited" {
					if err != nil || calls != 2 {
						t.Fatalf("safe retry failed: calls=%d err=%v", calls, err)
					}
				} else if err == nil || calls != 1 {
					t.Fatalf("unconfirmed send retried: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}
