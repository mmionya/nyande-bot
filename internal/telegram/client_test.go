package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenResponse struct{}

func (brokenResponse) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

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
