package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
)

type tikTokTransport func(*http.Request) (*http.Response, error)

func (f tikTokTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFilterTikTokVideos(t *testing.T) {
	inputs := []string{
		"https://www.tiktok.com/@cat/video/123?token=secret#x",
		"http://m.tiktok.com/@cat/video/123/",
		"https://tiktok.com/@dog/video/456",
		"https://www.tiktok.com/@cat", "https://www.tiktok.com/discover/cats",
		"https://www.tiktok.com.evil.test/@cat/video/789",
		"https://evil.test/@cat/video/789", "https://user@www.tiktok.com/@cat/video/789",
		"ftp://www.tiktok.com/@cat/video/789", "https://www.tiktok.com/@cat/video/notanid",
	}
	var results []searchResult
	for _, u := range inputs {
		results = append(results, searchResult{URL: u})
	}
	got := filterTikTokVideos(results)
	if len(got) != 2 || got[0].URL != "https://www.tiktok.com/@cat/video/123" || got[1].URL != "https://www.tiktok.com/@dog/video/456" {
		t.Fatalf("unexpected results: %+v", got)
	}
}

func TestTikTokSearchFallbackAndRequestCache(t *testing.T) {
	c := New(config.Config{LLMWebSearch: true, LLMBaseURL: "https://openrouter.ai/api/v1", LLMTimeout: time.Second})
	requests := 0
	c.http.Transport = tikTokTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		status := 200
		body := ""
		switch r.URL.Hostname() {
		case "openrouter.ai":
			status = 400
			body = `{"error":{"message":"Server tool request failed"}}`
		case "html.duckduckgo.com":
			body = `<a class="result__a" href="https://www.tiktok.com/discover/cats">Cats</a>`
		case "lite.duckduckgo.com":
			r.ParseForm()
			if !strings.Contains(r.Form.Get("q"), "site:tiktok.com inurl:video") {
				t.Error("missing scoped query")
			}
			body = `<a class="result-link" href="https://www.tiktok.com/@cat/video/123?tracking=1">Cat video</a>`
		default:
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	tool := c.MediaSearchTools()[0]
	for _, q := range []string{"cats", " CATS "} {
		result, err := tool.Execute(context.Background(), map[string]string{"query": q})
		if err != nil || !strings.Contains(result, "https://www.tiktok.com/@cat/video/123") || strings.Contains(result, "tracking=") {
			t.Fatalf("result=%q err=%v", result, err)
		}
	}
	if requests != 3 {
		t.Fatalf("cache/fallback: requests=%d", requests)
	}
}

func TestTikTokSearchUsesCitationsOnly(t *testing.T) {
	c := New(config.Config{LLMWebSearch: true, LLMBaseURL: "https://openrouter.ai/api/v1", LLMTimeout: time.Second})
	c.http.Transport = tikTokTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "openrouter.ai" {
			t.Fatalf("unexpected fallback: %s", r.URL)
		}
		body := `{"choices":[{"message":{"content":"https://www.tiktok.com/@invented/video/999","annotations":[{"type":"url_citation","url_citation":{"url":"https://www.tiktok.com/@real/video/123","title":"Real cat"}}]}}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	result := c.searchTikTok(context.Background(), "cats")
	if strings.Contains(result, "invented") || !strings.Contains(result, "@real/video/123") {
		t.Fatal(result)
	}
}

func TestTikTokSearchDisabled(t *testing.T) {
	c := New(config.Config{})
	if len(c.MediaSearchTools()) != 0 {
		t.Fatal("search exposed despite being disabled")
	}
}
