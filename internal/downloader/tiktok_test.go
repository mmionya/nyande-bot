package downloader

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestDownloadTikTokShareLinks(t *testing.T) {
	for _, test := range []struct {
		source, wantURL string
	}{
		{"https://www.tt.site/t/ZSgRvMXNu/", "https://www.tiktok.com/t/ZSgRvMXNu/"},
		{"https://tt.site/t/ZSgRvMXNu/", "https://www.tiktok.com/t/ZSgRvMXNu/"},
		{"www.tt.site/t/ZSgRvMXNu/", "https://www.tiktok.com/t/ZSgRvMXNu/"},
		{"https://WWW.TT.SITE/t/ZSgRvMXNu/?_r=1&_t=share", "https://www.tiktok.com/t/ZSgRvMXNu/?_r=1&_t=share"},
		{"https://www.tiktok.com/@cat/video/123", "https://www.tiktok.com/@cat/video/123"},
		{"https://vm.tiktok.com/ZSgRvMXNu/", "https://vm.tiktok.com/ZSgRvMXNu/"},
		{"https://vt.tiktok.com/ZSgRvMXNu/", "https://vt.tiktok.com/ZSgRvMXNu/"},
	} {
		t.Run(test.source, func(t *testing.T) {
			d := New(config.Config{RetryAttempts: 1, MaxMediaItems: 1, MaxFileSize: 1024})
			apiCalls, mediaCalls := 0, 0
			d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
				var body, contentType string
				switch request.URL.String() {
				case "https://www.tikwm.com/api/":
					apiCalls++
					if request.Method != http.MethodPost {
						t.Fatalf("TikWM method = %s, want POST", request.Method)
					}
					if err := request.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if got := request.PostForm.Get("url"); got != test.wantURL {
						t.Fatalf("TikWM URL = %q, want %q", got, test.wantURL)
					}
					body = `{"code":0,"data":{"hdplay":"https://8.8.8.8/video.mp4"}}`
					contentType = "application/json"
				case "https://8.8.8.8/video.mp4":
					mediaCalls++
					body, contentType = "video", "video/mp4"
				default:
					t.Fatalf("unexpected request: %s", request.URL)
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
			if apiCalls != 1 || mediaCalls != 1 {
				t.Fatalf("requests: TikWM = %d, media = %d; want one each", apiCalls, mediaCalls)
			}
			if result.Source != "tiktok" || len(result.Items) != 1 || result.Items[0].Kind != "video" || string(result.Items[0].Data) != "video" {
				t.Fatalf("unexpected download result: %#v", result)
			}
		})
	}
}
