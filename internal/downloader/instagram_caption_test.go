package downloader

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestInstagramApifyCaptionAttribution(t *testing.T) {
	for _, test := range []struct {
		name     string
		item     map[string]any
		fallback Post
		want     string
	}{
		{"owner", map[string]any{"caption": "Post", "ownerFullName": "Cat", "ownerUsername": "cat"}, Post{}, "Post\n\nCat (@cat)"},
		{"handle only", map[string]any{"caption": "Post", "ownerUsername": "cat"}, Post{}, "Post\n\n@cat"},
		{"nested owner", map[string]any{"caption": "Post", "owner": map[string]any{"full_name": "Cat", "username": "cat"}}, Post{}, "Post\n\nCat (@cat)"},
		{"no owner", map[string]any{"caption": "Post", "name": "Not an author"}, Post{}, "Post"},
		{"retain page text", map[string]any{"ownerUsername": "cat"}, Post{Text: "Page text"}, "Page text\n\n@cat"},
		{"retain page author", map[string]any{"caption": "API text"}, Post{Text: "Page text", Author: "@cat"}, "API text\n\n@cat"},
		{"retain both", map[string]any{}, Post{Text: "Page text", Author: "@cat"}, "Page text\n\n@cat"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := instagramApifyCaption(test.item, test.fallback); got != test.want {
				t.Fatalf("caption=%q, want %q", got, test.want)
			}
		})
	}
	caption := instagramApifyCaption(map[string]any{"caption": strings.Repeat("я", 1500), "ownerUsername": "cat"}, Post{})
	if len([]rune(caption)) > mediaCaptionLimit || !strings.HasSuffix(caption, "…\n\n@cat") {
		t.Fatalf("long caption lost attribution: %q", caption)
	}
}

func TestInstagramPageAuthorUsesExplicitAttribution(t *testing.T) {
	comments := `<script>{"comments":[{"owner":{"username":"commenter"}}]}</script>`
	if got := instagramPageAuthor(comments); got != "" {
		t.Fatalf("commenter used as post author: %q", got)
	}
	if got := instagramPageAuthor(comments + `<meta name="author" content="Alice &amp; Bob (@cats)">`); got != "Alice & Bob (@cats)" {
		t.Fatalf("explicit author lost: %q", got)
	}
}

func TestInstagramCaptionKeepsMetadataAcrossFallbacks(t *testing.T) {
	if !isPublicURL(context.Background(), &url.URL{Scheme: "https", Host: "www.instagram.com"}) {
		t.Skip("public Instagram DNS unavailable; HTTP requests are mocked")
	}
	for _, stage := range []string{"page", "embed", "apify"} {
		t.Run(stage, func(t *testing.T) {
			d := New(config.Config{MaxFileSize: 1024, MaxMediaItems: 1, YTDownloadPath: filepath.Join(t.TempDir(), "missing-yt-dlp"), APIFYToken: "fake"})
			mediaCalls := 0
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				body, mime := "", "text/html"
				switch {
				case r.URL.Hostname() == "api.apify.com":
					if stage != "apify" {
						t.Fatal("unexpected Apify request")
					}
					body, mime = `[{"ownerUsername":"cat","result":[{"url":"https://www.instagram.com/photo.jpg"}]}]`, "application/json"
				case r.URL.Path == "/p/ABC123/":
					body = `<meta property="og:description" content="Full page text">`
					if stage == "page" {
						body += `<meta name="author" content="@cat"><meta property="og:image" content="https://www.instagram.com/photo.jpg">`
					}
				case r.URL.Path == "/p/ABC123/embed/captioned/":
					if stage == "embed" {
						body = `<meta name="author" content="@cat"><meta property="og:image" content="https://www.instagram.com/photo.jpg">`
					}
				case r.URL.Path == "/photo.jpg":
					mediaCalls++
					body, mime = "photo bytes", "image/jpeg"
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			value, _ := url.Parse("https://www.instagram.com/p/ABC123/")
			result, err := d.downloadInstagram(context.Background(), value)
			if err != nil || result.Caption != "Full page text\n\n@cat" || len(result.Items) != 1 || mediaCalls != 1 {
				t.Fatalf("result=%+v media calls=%d err=%v", result, mediaCalls, err)
			}
		})
	}
}
