package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/config"
)

func twitterTestDownloader(t *testing.T, body string) (*Downloader, *int) {
	t.Helper()
	calls := 0
	d := New(config.Config{RetryAttempts: 1, MaxFileSize: 1024, MaxMediaItems: 10})
	d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.String() != "https://api.fxtwitter.com/status/123" {
			t.Errorf("unexpected metadata request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	return d, &calls
}

func TestTwitterTextPostsKeepFullTextAndAttribution(t *testing.T) {
	full := "First &amp; second\n\n" + strings.Repeat("длинный текст ", 150)
	for _, media := range []string{"", `,"media":null`, `,"media":{}`, `,"media":{"photos":[],"videos":[]}`} {
		encoded, err := json.Marshal(full)
		if err != nil {
			t.Fatal(err)
		}
		body := `{"code":200,"tweet":{"id":"123","text":` + string(encoded) + `,"author":{"name":"Alice &amp; Bob","screen_name":"alice"}` + media + `}}`
		d, calls := twitterTestDownloader(t, body)
		result, err := d.Download(context.Background(), "https://twitter.com/alice/status/123?s=20#fragment")
		if err != nil || len(result.Items) != 0 || result.Post == nil || *calls != 1 {
			t.Fatalf("result=%+v calls=%d err=%v", result, *calls, err)
		}
		if result.Post.Text != strings.TrimSpace(strings.ReplaceAll(full, "&amp;", "&")) || result.Post.Author != "Alice & Bob (@alice)" || result.Post.URL != "https://x.com/alice/status/123" || result.Source != "twitter" {
			t.Fatalf("post=%+v source=%s", result.Post, result.Source)
		}
		if utf8.RuneCountInString(result.Caption) > 1024 || utf8.RuneCountInString(result.Post.Text) <= 1024 {
			t.Fatalf("caption/full text sizes: %d/%d", utf8.RuneCountInString(result.Caption), utf8.RuneCountInString(result.Post.Text))
		}
	}
}

func TestTwitterPostMetadataDoesNotDownloadMedia(t *testing.T) {
	d, calls := twitterTestDownloader(t, `{"code":200,"tweet":{"id":"123","text":"Translate this","author":{"screen_name":"alice"},"media":{"videos":[{"url":"https://video.twimg.com/movie.mp4"}]}}}`)
	post, err := d.TwitterPost(context.Background(), "https://x.com/i/status/123")
	if err != nil || post.Text != "Translate this" || post.Author != "(@alice)" || post.URL != "https://x.com/i/status/123" || *calls != 1 {
		t.Fatalf("post=%+v calls=%d err=%v", post, *calls, err)
	}
}

func TestTwitterPostCanonicalURLs(t *testing.T) {
	for _, test := range []struct{ raw, canonical string }{
		{"https://x.com/alice/status/123?s=20#fragment", "https://x.com/alice/status/123"},
		{"http://mobile.twitter.com/alice/status/123/", "https://x.com/alice/status/123"},
		{"twitter.com/alice/status/123/photo/2", "https://x.com/alice/status/123"},
		{"https://www.x.com/alice/status/123/video/1", "https://x.com/alice/status/123"},
		{"https://x.com/i/web/status/123", "https://x.com/i/status/123"},
		{"https://x.com/i/status/123", "https://x.com/i/status/123"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			d, calls := twitterTestDownloader(t, `{"code":200,"tweet":{"id":"123","text":"A post"}}`)
			post, err := d.TwitterPost(context.Background(), test.raw)
			if err != nil || post.URL != test.canonical || *calls != 1 {
				t.Fatalf("post=%+v calls=%d err=%v", post, *calls, err)
			}
		})
	}
	for _, raw := range []string{
		"https://x.com/alice", "https://x.com/alice/status/nope", "https://x.com/alice/status/0",
		"https://x.com/alice/status/123extra", "https://x.com/alice/status/123/unrelated",
		"https://x.com/alice/status/18446744073709551616", "https://example.com/alice/status/123",
		"https://x.com.evil.test/alice/status/123", "https://alice@x.com/alice/status/123",
		"https://x.com:8443/alice/status/123", "file:///alice/status/123",
	} {
		t.Run(raw, func(t *testing.T) {
			d, calls := twitterTestDownloader(t, `{"code":200,"tweet":{"id":"123","text":"A post"}}`)
			if _, err := d.TwitterPost(context.Background(), raw); err == nil || *calls != 0 {
				t.Fatalf("invalid URL fetched metadata: calls=%d err=%v", *calls, err)
			}
		})
	}
}

func TestTwitterInvalidOrUnavailableMediaIsNotATextPost(t *testing.T) {
	for _, body := range []string{
		`{`, `null`, `{}`, `{"code":404}`, `{"code":200}`, `{"code":200,"tweet":null}`,
		`{"code":200,"tweet":{"id":"456","text":"Wrong post"}}`,
		`{"code":200,"tweet":{"text":"Missing ID"}}`,
		`{"code":200,"tweet":{"id":"123","text":""}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":"invalid"}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":{"photos":"invalid"}}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":{"photos":[{}]}}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":{"photos":[null]}}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":{"photos":[{"url":"https://example.com/pic.jpg"}]}}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":{"external":{"url":"https://example.com/clip"}}}}`,
		`{"code":200,"tweet":{"id":"123","text":"Text","media":{"new_media_type":{}}}}`,
	} {
		d, calls := twitterTestDownloader(t, body)
		result, err := d.Download(context.Background(), "https://x.com/alice/status/123")
		if err == nil || result.Post != nil || *calls != 1 {
			t.Fatalf("invalid response became a text post: body=%s result=%+v calls=%d err=%v", body, result, *calls, err)
		}
	}
}

func TestTwitterMediaDownloadAndFailures(t *testing.T) {
	mediaURL, _ := url.Parse("https://pbs.twimg.com/photo.jpg")
	if !isPublicURL(context.Background(), mediaURL) {
		t.Skip("public Twitter CDN DNS unavailable; HTTP requests are mocked")
	}
	for _, fail := range []bool{false, true} {
		d := New(config.Config{RetryAttempts: 1, MaxFileSize: 1024, MaxMediaItems: 10})
		metadata, media := 0, 0
		d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
			body, contentType, status := "", "application/json", 200
			if r.URL.Hostname() == "api.fxtwitter.com" {
				metadata++
				body = `{"code":200,"tweet":{"id":"123","text":"Caption","author":{"name":"Alice","screen_name":"alice"},"media":{"photos":[{"url":"https://pbs.twimg.com/photo.jpg"},{"url":"https://pbs.twimg.com/photo.jpg"}]}}}`
			} else if r.URL.String() == mediaURL.String() {
				media++
				body, contentType = "image bytes", "image/jpeg"
				if fail {
					status = 403
				}
			} else {
				t.Errorf("unexpected media request: %s", r.URL)
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})
		result, err := d.Download(context.Background(), "https://x.com/alice/status/123")
		if metadata != 1 || media != 1 || (err != nil) != fail {
			t.Fatalf("fail=%t metadata=%d media=%d err=%v", fail, metadata, media, err)
		}
		if !fail && (len(result.Items) != 1 || result.Items[0].Kind != "photo" || string(result.Items[0].Data) != "image bytes" || result.Caption != "Caption Alice (@alice)" || result.Post == nil || result.Post.Text != "Caption") {
			t.Fatalf("media regression: %+v", result)
		}
		if fail && result.Post != nil {
			t.Fatalf("failed media download became a text post: %+v", result)
		}
	}
}

func TestTwitterPostCancellationAndHTTPFailure(t *testing.T) {
	d, calls := twitterTestDownloader(t, `{"code":200,"tweet":{"id":"123","text":"A post"}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.TwitterPost(ctx, "https://x.com/alice/status/123"); !errors.Is(err, context.Canceled) || *calls != 0 {
		t.Fatalf("calls=%d err=%v", *calls, err)
	}
	d.cfg.RetryAttempts = 2
	d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: r}, nil
	})
	if _, err := d.TwitterPost(context.Background(), "https://x.com/alice/status/123"); err == nil || *calls != 2 {
		t.Fatalf("calls=%d err=%v", *calls, err)
	}
}
