package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
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
		if !strings.HasPrefix(result.Caption, "First & second\n\n") || !strings.HasSuffix(result.Caption, "…\n\nAlice & Bob (@alice)") {
			t.Fatalf("caption lost paragraphs or author: %q", result.Caption)
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

func TestTwitterMediaURLsPreserveAnimations(t *testing.T) {
	for _, test := range []struct {
		name, media string
		want        []twitterMedia
	}{
		{
			name:  "videos retain explicit GIF types",
			media: `{"videos":[{"url":"one.mp4","type":"gif"},{"url":"two.mp4","type":"animated_gif"},{"url":"three.mp4","type":"video"},{"url":"four.mp4"}]}`,
			want:  []twitterMedia{{"one.mp4", "animation"}, {"two.mp4", "animation"}, {"three.mp4", "video"}, {"four.mp4", "video"}},
		},
		{
			name:  "all preserves each supported type",
			media: `{"all":[{"url":"one.mp4","type":"gif"},{"url":"two.mp4","type":"animated_gif"},{"url":"three.mp4","type":"video"},{"url":"four.jpg","type":"photo"},{"url":"other","type":"unknown"}]}`,
			want:  []twitterMedia{{"one.mp4", "animation"}, {"two.mp4", "animation"}, {"three.mp4", "video"}, {"four.jpg", "photo"}},
		},
		{
			name:  "all upgrades duplicate video to GIF and retains missing media",
			media: `{"videos":[{"url":"one.mp4","type":"video"},{"url":"one.mp4"}],"photos":[{"url":"two.jpg"}],"all":[{"url":"one.mp4","type":"gif"},{"url":"two.jpg","type":"photo"},{"url":"three.mp4","type":"animated_gif"}]}`,
			want:  []twitterMedia{{"one.mp4", "animation"}, {"two.jpg", "photo"}, {"three.mp4", "animation"}},
		},
		{
			name:  "generic duplicates cannot downgrade GIF",
			media: `{"videos":[{"url":"one.mp4","type":"gif"},{"url":"one.mp4","type":"video"}],"all":[{"url":"one.mp4","type":"video"}]}`,
			want:  []twitterMedia{{"one.mp4", "animation"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, declared, err := twitterMediaURLs(json.RawMessage(test.media))
			if err != nil || !declared || !slices.Equal(got, test.want) {
				t.Fatalf("media=%+v declared=%t err=%v; want %+v", got, declared, err, test.want)
			}
		})
	}
}

func TestTwitterAnimationDownloadPreservesVideoValidationAndLimits(t *testing.T) {
	mediaURL, _ := url.Parse("https://video.twimg.com/clip.mp4")
	if !isPublicURL(context.Background(), mediaURL) {
		t.Skip("public Twitter CDN DNS unavailable; HTTP requests are mocked")
	}
	for _, test := range []struct {
		name, mediaType, contentType, wantKind string
		maxFile, maxSource                     int64
		wantError, compressionError            bool
	}{
		{"GIF MP4", "gif", "video/mp4", "animation", 1024, 2048, false, false},
		{"animated GIF MP4", "animated_gif", "video/mp4", "animation", 1024, 2048, false, false},
		{"ordinary video", "video", "video/mp4", "video", 1024, 2048, false, false},
		{"GIF rejects photo response", "gif", "image/jpeg", "", 1024, 2048, true, false},
		{"GIF respects video source limit", "gif", "video/mp4", "", 4, 8, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := New(config.Config{
				RetryAttempts: 1, MaxFileSize: test.maxFile, MaxMediaItems: 10,
				VideoCompressionEnabled: true, VideoMaxSourceSize: test.maxSource,
			})
			mediaCalls := 0
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				body, contentType := "video bytes", test.contentType
				if r.URL.Hostname() == "api.fxtwitter.com" {
					body = `{"code":200,"tweet":{"id":"123","media":{"videos":[{"url":"` + mediaURL.String() + `","type":"` + test.mediaType + `"}]}}}`
					contentType = "application/json"
				} else if r.URL.String() == mediaURL.String() {
					mediaCalls++
				} else {
					t.Errorf("unexpected media request: %s", r.URL)
				}
				return &http.Response{
					StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, ContentLength: int64(len(body)),
					Body: io.NopCloser(strings.NewReader(body)), Request: r,
				}, nil
			})
			result, err := d.Download(context.Background(), "https://x.com/alice/status/123")
			if (err != nil) != test.wantError || errors.Is(err, ErrVideoCompression) != test.compressionError || mediaCalls != 1 {
				t.Fatalf("mediaCalls=%d err=%v", mediaCalls, err)
			}
			if !test.wantError && (len(result.Items) != 1 || result.Items[0].Kind != test.wantKind || result.Items[0].MIME != "video/mp4" || result.Items[0].Name != "clip.mp4" || string(result.Items[0].Data) != "video bytes") {
				t.Fatalf("animation/video metadata lost: %+v", result.Items)
			}
		})
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
		if !fail && (len(result.Items) != 1 || result.Items[0].Kind != "photo" || string(result.Items[0].Data) != "image bytes" || result.Caption != "Caption\n\nAlice (@alice)" || result.Post == nil || result.Post.Text != "Caption") {
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
