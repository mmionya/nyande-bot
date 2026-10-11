package downloader

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestResolveRedditGalleryURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://www.reddit.com/gallery/1w5nkpp", "https://www.reddit.com/comments/1w5nkpp/"},
		{"https://www.reddit.com/gallery/1w5nkpp/?share=1", "https://www.reddit.com/comments/1w5nkpp/?share=1"},
		{"https://old.reddit.com/gallery/1w5nkpp", "https://old.reddit.com/comments/1w5nkpp/"},
		{"https://www.reddit.com/r/cats/comments/abc/title/", "https://www.reddit.com/r/cats/comments/abc/title/"},
		{"https://www.reddit.com/gallery/", "https://www.reddit.com/gallery/"},
		{"https://reddit.com.example.org/gallery/1w5nkpp", "https://reddit.com.example.org/gallery/1w5nkpp"},
	} {
		t.Run(test.input, func(t *testing.T) {
			input, err := url.Parse(test.input)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := (&Downloader{}).resolveRedditURL(context.Background(), input)
			if err != nil || resolved.String() != test.want {
				t.Fatalf("resolved = %v, err = %v; want %s", resolved, err, test.want)
			}
			if input.String() != test.input {
				t.Fatal("resolution modified the input URL")
			}
		})
	}
}

func TestRedditDirectMediaSkipsPageExtraction(t *testing.T) {
	if !isPublicURL(context.Background(), &url.URL{Scheme: "https", Host: "i.redd.it"}) {
		t.Skip("public Reddit DNS unavailable; HTTP requests are mocked")
	}
	for _, test := range []struct{ extension, contentType, kind string }{
		{"jpg", "image/jpeg", "photo"},
		{"gif", "image/gif", "animation"},
		{"mp4", "video/mp4", "video"},
	} {
		t.Run(test.extension, func(t *testing.T) {
			d := New(config.Config{MaxFileSize: 1024, MaxMediaItems: 1, RetryAttempts: 1, YTDownloadPath: filepath.Join(t.TempDir(), "missing-yt-dlp")})
			calls := 0
			path := "/original." + test.extension
			d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodGet || request.URL.Path != path {
					t.Fatalf("direct media must need only its GET, got %s %s", request.Method, request.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {test.contentType}}, Body: io.NopCloser(strings.NewReader("media")), Request: request}, nil
			})
			result, err := d.Download(context.Background(), "https://i.redd.it"+path)
			if err != nil || calls != 1 || result.Source != "reddit" || len(result.Items) != 1 || result.Items[0].Kind != test.kind {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

func TestRedditGalleryStopsAtSuccessfulMediaLimit(t *testing.T) {
	if !isPublicURL(context.Background(), &url.URL{Scheme: "https", Host: "i.redd.it"}) {
		t.Skip("public Reddit DNS unavailable; HTTP requests are mocked")
	}
	d := New(config.Config{MaxFileSize: 1024, MaxMediaItems: 2, RetryAttempts: 1, YTDownloadPath: filepath.Join(t.TempDir(), "missing-yt-dlp")})
	var paths []string
	d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		status, contentType, body := http.StatusOK, "image/jpeg", "jpg"
		switch request.URL.Path {
		case "/comments/abc.json":
			contentType = "application/json"
			body = `[{"data":{"children":[{"data":{"title":"Gallery caption","gallery_data":{"items":[{"media_id":"a"},{"media_id":"b"},{"media_id":"c"},{"media_id":"d"}]},"media_metadata":{"a":{"s":{"u":"https://i.redd.it/a.jpg"}},"b":{"s":{"u":"https://i.redd.it/b.jpg"}},"c":{"s":{"u":"https://i.redd.it/c.jpg"}},"d":{"s":{"u":"https://i.redd.it/d.jpg"}}}}}]}}]`
		case "/b.jpg":
			status = http.StatusForbidden
		case "/a.jpg", "/c.jpg":
		default:
			t.Fatalf("unnecessary media request: %s", request.URL)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	result, err := d.Download(context.Background(), "https://www.reddit.com/gallery/abc/")
	if err != nil || result.Source != "reddit" || result.Caption != "Gallery caption" || len(result.Items) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !slices.Equal(paths, []string{"/comments/abc.json", "/a.jpg", "/b.jpg", "/c.jpg"}) || result.Items[0].Name != "a.jpg" || result.Items[1].Name != "c.jpg" {
		t.Fatalf("gallery limit must preserve successful item order: paths=%v items=%+v", paths, result.Items)
	}
}

func TestRedditImageURLsIgnoreVideoThumbnails(t *testing.T) {
	for _, test := range []struct {
		name string
		post string
		want []string
	}{
		{
			name: "native video",
			post: `{"is_video":true,"url":"https://v.redd.it/video","preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "external video",
			post: `{"post_hint":"rich:video","url":"https://youtu.be/video","preview":{"images":[{"source":{"url":"https://external-preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "crossposted video",
			post: `{"is_video":false,"url":"https://v.redd.it/video","crosspost_parent_list":[{"is_video":true}],"preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "animation",
			post: `{"post_hint":"image","url":"https://i.redd.it/animation.gif","preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "original image",
			post: `{"post_hint":"image","url_overridden_by_dest":"https://i.redd.it/original.png","preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
			want: []string{"https://i.redd.it/original.png"},
		},
		{
			name: "gallery preserves order",
			post: `{"gallery_data":{"items":[{"media_id":"b"},{"media_id":"a"}]},"media_metadata":{"a":{"s":{"u":"https://i.redd.it/a.png"}},"b":{"s":{"u":"https://preview.redd.it/b.jpg?width=100&amp;format=pjpg"}}}}`,
			want: []string{"https://preview.redd.it/b.jpg?width=100&format=pjpg", "https://i.redd.it/a.png"},
		},
		{
			name: "untrusted image host",
			post: `{"url":"https://i.redd.it.example.org/image.jpg"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var post map[string]any
			if err := json.Unmarshal([]byte(test.post), &post); err != nil {
				t.Fatal(err)
			}
			if got := redditImageURLs(post); !slices.Equal(got, test.want) {
				t.Fatalf("redditImageURLs() = %v; want %v", got, test.want)
			}
		})
	}
}
