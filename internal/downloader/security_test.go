package downloader

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/publichttp"
)

func TestSocialRedirectsRejectPrivateTargetsBeforeDispatch(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	for _, test := range []struct {
		name string
		run  func(*Downloader) error
	}{
		{"Pinterest short link", func(d *Downloader) error {
			value, _ := url.Parse("https://pin.it/example")
			_, err := d.resolvePinterestURL(context.Background(), value)
			return err
		}},
		{"Reddit short link", func(d *Downloader) error {
			value, _ := url.Parse("https://redd.it/example")
			_, err := d.resolveRedditURL(context.Background(), value)
			return err
		}},
		{"Xiaohongshu page", func(d *Downloader) error {
			value, _ := url.Parse("https://www.xiaohongshu.com/explore/abc")
			_, _, err := d.fetchXiaohongshuPage(context.Background(), value)
			return err
		}},
		{"Instagram page", func(d *Downloader) error {
			_, err := d.fetchInstagramPage(context.Background(), "https://www.instagram.com/p/abc/")
			return err
		}},
		{"Pinterest API", func(d *Downloader) error {
			_, err := d.fetchPinterestPin(context.Background(), "123", "https://www.pinterest.com/pin/123/")
			return err
		}},
		{"TikWM API", func(d *Downloader) error {
			value, _ := url.Parse("https://www.tiktok.com/@cat/video/123")
			_, err := d.downloadTikTok(context.Background(), value)
			return err
		}},
		{"FxTwitter API", func(d *Downloader) error {
			_, _, err := d.fetchTwitterPost(context.Background(), "https://x.com/cat/status/123", "123")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, target := range []string{server.URL, "http://100.100.100.200/private"} {
				d := New(config.Config{})
				requests := 0
				d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					if requests > 1 {
						t.Fatal("private redirect dispatched to transport")
					}
					return &http.Response{StatusCode: http.StatusSeeOther, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
				})
				if err := test.run(d); !errors.Is(err, publichttp.ErrUnsafeAddress) || requests != 1 {
					t.Fatalf("target=%s requests=%d err=%v", target, requests, err)
				}
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("private endpoint received %d requests", hits.Load())
	}
}

func TestMediaRejectsSharedAddressSpaceBeforeDispatch(t *testing.T) {
	d := New(config.Config{MaxFileSize: 1024})
	d.client.Transport = mediaRedirectTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("shared address reached transport")
		return nil, nil
	})
	for _, raw := range []string{"http://100.100.100.200/cat.jpg", "http://100.64.0.1/video.mp4", "http://[::ffff:100.127.255.255]/cat.jpg"} {
		if _, err := d.fetchMedia(context.Background(), raw, ""); err == nil {
			t.Fatalf("shared address accepted: %s", raw)
		}
	}
}

func TestMediaCollectionHasAggregateByteLimit(t *testing.T) {
	d := New(config.Config{MaxFileSize: 1024 * 1024})
	item := Media{Data: make([]byte, 1024*1024)}
	var items []Media
	var err error
	for range 100 {
		items, err = d.appendMedia(items, item)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.appendMedia(items, Media{Data: []byte{1}}); !errors.Is(err, errMediaResultTooLarge) || retryable(err) || !terminalDownloadError(err) {
		t.Fatalf("aggregate overflow was not terminal: %v", err)
	}
	d.cfg.MaxFileSize = maxMediaResultBytes + 1
	if _, err := d.appendMedia(items, Media{Data: []byte{1}}); err != nil {
		t.Fatalf("explicit larger single-file limit ignored: %v", err)
	}
}

func TestJSONResponseLimitChecksEntireBody(t *testing.T) {
	var result map[string]any
	if err := decodeLimitedJSON(strings.NewReader(`{"ok":true}`), 11, &result); err != nil {
		t.Fatal(err)
	}
	// A complete small JSON object followed by excess whitespace used to pass
	// json.Decoder while keeping the response size effectively unbounded.
	if err := decodeLimitedJSON(strings.NewReader(`{"ok":true}`+strings.Repeat(" ", 100)), 11, &result); err == nil {
		t.Fatal("valid JSON prefix bypassed the byte limit")
	}
}
