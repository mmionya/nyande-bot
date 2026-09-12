package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
)

func TestDownloadMediaToolReportsActualDeliveryAndDoesNotRepeat(t *testing.T) {
	for _, test := range []struct {
		name  string
		count int
		err   error
		want  string
	}{
		{"success", 2, nil, "Successfully sent 2"},
		{"partial", 1, errors.New("Telegram failed"), "Only 1 media files"},
		{"failure", 0, errors.New("HTTP 403"), "delivery was not confirmed"},
		{"empty", 0, nil, "No media files were sent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			tool := newDownloadMediaTool(func(ctx context.Context, rawURL string) (int, error) {
				calls++
				if rawURL != "https://example.com/cat.mp4" {
					t.Errorf("unexpected URL: %s", rawURL)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Minute {
					t.Error("download has no bounded deadline")
				}
				return test.count, test.err
			})
			result, err := tool.Execute(context.Background(), map[string]string{"url": " https://example.com/cat.mp4#video "})
			if err != nil || !strings.Contains(result, test.want) {
				t.Fatalf("result=%q err=%v", result, err)
			}
			repeat, err := tool.Execute(context.Background(), map[string]string{"url": "https://example.com/cat.mp4"})
			if err != nil || calls != 1 || !strings.Contains(repeat, "already attempted") || !strings.Contains(repeat, result) {
				t.Fatalf("repeat=%q err=%v calls=%d", repeat, err, calls)
			}
		})
	}
}

func TestDownloadMediaToolRejectsInvalidAndPrivateURLs(t *testing.T) {
	tool := newDownloadMediaTool(func(context.Context, string) (int, error) { t.Fatal("invalid URL reached downloader"); return 0, nil })
	for _, raw := range []string{"", "cats playing", "file:///tmp/cat.jpg", "https://user:secret@example.com/cat.jpg", "http://127.0.0.1/cat.jpg", "http://[::1]/cat.mp4", "http://169.254.169.254/cat.jpg", "http://10.0.0.1/cat.jpg", "http://localhost/cat.jpg", "https://example.com/article"} {
		result, err := tool.Execute(context.Background(), map[string]string{"url": raw})
		if err != nil || !strings.Contains(result, "Cannot download") {
			t.Errorf("%q: %q %v", raw, result, err)
		}
	}
	for _, raw := range []string{"https://youtu.be/abcdefghijk", "https://www.pinterest.com/pin/123/", "https://www.reddit.com/gallery/abc", "https://example.com/cat.jpg?size=large"} {
		if _, err := mediaToolURL(raw); err != nil {
			t.Errorf("supported URL rejected: %s: %v", raw, err)
		}
	}
}

func TestDownloadMediaToolHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := newDownloadMediaTool(func(context.Context, string) (int, error) {
		t.Fatal("cancelled request reached downloader")
		return 0, nil
	})
	if _, err := tool.Execute(ctx, map[string]string{"url": "https://example.com/cat.jpg"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestLLMCanCallDownloadMediaAndReceiveDeliveryResult(t *testing.T) {
	requests, deliveries := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var payload struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if requests == 1 {
			found := false
			for _, tool := range payload.Tools {
				found = found || tool.Function.Name == "download_media"
			}
			if !found {
				t.Error("download tool was not advertised")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"download-1","type":"function","function":{"name":"download_media","arguments":"{\"url\":\"https://example.com/cat.mp4\"}"}}]}}]}`)
			return
		}
		last := payload.Messages[len(payload.Messages)-1]
		if last.Role != "tool" || !strings.Contains(last.Content, "Successfully sent 1") {
			t.Errorf("missing delivery observation: %#v", last)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"Видео отправлено"}}]}`)
	}))
	defer server.Close()
	client := llm.New(config.Config{LLMEnabled: true, LLMBaseURL: server.URL, LLMModel: "test", LLMTimeout: time.Second})
	tool := newDownloadMediaTool(func(context.Context, string) (int, error) { deliveries++; return 1, nil })
	answer, err := client.Ask(context.Background(), llm.Request{ChatID: 1, Text: "Пришли видео", Tools: []llm.Tool{tool}})
	if err != nil || answer != "Видео отправлено" || deliveries != 1 || requests != 2 {
		t.Fatalf("answer=%q err=%v deliveries=%d requests=%d", answer, err, deliveries, requests)
	}
}
