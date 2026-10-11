package downloader

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestSocialDownloadsStopAtMediaLimit(t *testing.T) {
	for _, test := range []struct {
		name, source, endpoint, payload string
		wantPaths                       []string
	}{
		{
			name: "TikTok photos", source: "https://www.tiktok.com/@cat/photo/123", endpoint: "/api/",
			payload:   `{"code":0,"data":{"images":["http://8.8.8.8/invalid.jpg","https://8.8.8.8/one.jpg","https://8.8.8.8/two.jpg","https://8.8.8.8/unused.jpg"]}}`,
			wantPaths: []string{"/one.jpg", "/two.jpg"},
		},
		{
			name: "X mixed media", source: "https://x.com/cat/status/123", endpoint: "/cat/status/123",
			payload:   `{"tweet":{"media":{"videos":[{"url":"https://untrusted.example/invalid.mp4"},{"url":"https://video.twimg.com/one.mp4"},{"url":"https://video.twimg.com/one.mp4"}],"photos":[{"url":"http://pbs.twimg.com/invalid.jpg"},{"url":"https://pbs.twimg.com/two.jpg"},{"url":"https://pbs.twimg.com/unused.jpg"}]}}}`,
			wantPaths: []string{"/one.mp4", "/two.jpg"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strings.HasPrefix(test.source, "https://x.com/") {
				for _, raw := range []string{"https://video.twimg.com", "https://pbs.twimg.com"} {
					value, _ := url.Parse(raw)
					if !isPublicURL(context.Background(), value) {
						t.Skip("public X media DNS unavailable; HTTP requests are mocked")
					}
				}
			}
			d := New(config.Config{MaxFileSize: 1024, MaxMediaItems: 2, RetryAttempts: 1})
			var paths []string
			d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				contentType, body := "image/jpeg", "image"
				if request.URL.Path == test.endpoint {
					contentType, body = "application/json", test.payload
				} else if strings.HasSuffix(request.URL.Path, ".mp4") {
					contentType, body = "video/mp4", "video"
				}
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
					Body: io.NopCloser(strings.NewReader(body)), Request: request,
				}, nil
			})
			result, err := d.Download(context.Background(), test.source)
			if err != nil {
				t.Fatal(err)
			}
			if want := append([]string{test.endpoint}, test.wantPaths...); !slices.Equal(paths, want) {
				t.Fatalf("requests = %v, want %v", paths, want)
			}
			var names []string
			for _, item := range result.Items {
				names = append(names, "/"+item.Name)
			}
			if !slices.Equal(names, test.wantPaths) {
				t.Fatalf("items = %v, want %v", names, test.wantPaths)
			}
		})
	}
}
