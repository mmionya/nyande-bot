package downloader

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

type mediaRedirectTransport func(*http.Request) (*http.Response, error)

func (f mediaRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaRejectsPrivateRedirectBeforeConnecting(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/cat.jpg", "http://169.254.169.254/cat.jpg", "http://[fe80::1]/cat.jpg"} {
		requests := 0
		d := &Downloader{client: &http.Client{Transport: mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})}}
		_, err := d.fetchMedia(context.Background(), "https://8.8.8.8/cat.jpg", "photo")
		if err == nil || !strings.Contains(err.Error(), "non-public") || requests != 1 {
			t.Fatalf("target=%s requests=%d err=%v", target, requests, err)
		}
	}
}

func TestMediaRejectsErrorPagesAtVideoURLs(t *testing.T) {
	for _, contentType := range []string{"text/html; charset=utf-8", "application/json", "application/octet-stream", "video/mp4", ""} {
		t.Run(contentType, func(t *testing.T) {
			d := New(config.Config{MaxFileSize: 1024})
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader("data")), Request: r}, nil
			})
			_, err := d.fetchMedia(context.Background(), "https://8.8.8.8/video.mp4", "video")
			wantError := strings.HasPrefix(contentType, "text/html") || contentType == "application/json"
			if (err != nil) != wantError {
				t.Fatalf("fetchMedia() error = %v, wantError = %t", err, wantError)
			}
		})
	}
}
