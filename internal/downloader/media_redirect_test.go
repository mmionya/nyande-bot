package downloader

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
