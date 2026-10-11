package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestInstagramDownloadsJPEGAtHEICURLs(t *testing.T) {
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint("invalid=", invalid), func(t *testing.T) {
			directory := t.TempDir()
			data := photo.Bytes()
			if invalid {
				data = []byte("<html>Login required</html>")
			}
			if err := os.WriteFile(filepath.Join(directory, "photo"), data, 0600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(directory, "yt-dlp")
			if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
    if [ "$1" = --output ]; then
        directory=${2%/*}
        cp "${0%/*}/photo" "$directory/00002-second.heic"
        cp "${0%/*}/photo" "$directory/00001-first.heic"
        exit 0
    fi
    shift
done
exit 1
`), 0700); err != nil {
				t.Fatal(err)
			}
			d := New(config.Config{YTDownloadPath: script, MaxFileSize: 1024 * 1024, MaxMediaItems: 10})
			value, _ := url.Parse("https://www.instagram.com/p/example/")
			result, err := d.downloadYTDLPWithOptions(context.Background(), value, true)
			if invalid {
				if err == nil || len(result.Items) != 0 {
					t.Fatal("non-image contents must not be delivered as a photo")
				}
				return
			}
			if err != nil || len(result.Items) != 2 {
				t.Fatalf("expected both carousel photos: items=%d err=%v", len(result.Items), err)
			}
			for index, name := range []string{"00001-first.jpg", "00002-second.jpg"} {
				item := result.Items[index]
				if item.Name != name || item.Kind != "photo" || item.MIME != "image/jpeg" || !bytes.Equal(item.Data, data) {
					t.Fatalf("unexpected photo %d: name=%q kind=%q MIME=%q bytes=%d", index, item.Name, item.Kind, item.MIME, len(item.Data))
				}
			}
		})
	}
}

func TestInstagramPageMediaExtractsEscapedCarouselImages(t *testing.T) {
	page := `<meta content="A photo caption &amp; more" property="og:description">
		<script>
		display_url\":\"https:\\/\\/scontent.cdninstagram.com\\/one.jpg?x=1\\u0026y=2\"
		display_url\\\":\\\"https:\\\\/\\\\/scontent.cdninstagram.com\\\\/two.jpg\"
		display_url\":\"https:\\/\\/scontent.cdninstagram.com\\/one.jpg?x=1\\u0026y=2\"
		</script>`

	urls, caption := instagramPageMedia(page, true)
	expected := []string{
		"https://scontent.cdninstagram.com/one.jpg?x=1&y=2",
		"https://scontent.cdninstagram.com/two.jpg",
	}
	if !reflect.DeepEqual(urls, expected) {
		t.Fatalf("unexpected Instagram image URLs:\nwant: %#v\n got: %#v", expected, urls)
	}
	if caption != "A photo caption & more" {
		t.Fatalf("unexpected caption: %q", caption)
	}
}

func TestInstagramPageMediaPrefersVideoOverPreview(t *testing.T) {
	page := `<meta property="og:image" content="https://scontent.cdninstagram.com/preview.jpg">
		<script>{"video_url":"https:\/\/scontent.cdninstagram.com\/clip.mp4"}</script>`

	urls, _ := instagramPageMedia(page, false)
	expected := []string{"https://scontent.cdninstagram.com/clip.mp4"}
	if !reflect.DeepEqual(urls, expected) {
		t.Fatalf("expected video without preview image, got %#v", urls)
	}
}

func TestInstagramPageMediaExtractsPhotoCaptionFromJSON(t *testing.T) {
	page := `<script>{"display_url":"https:\/\/scontent.cdninstagram.com\/photo.jpg","caption_text":"Photo caption from Instagram"}</script>`

	urls, caption := instagramPageMedia(page, true)
	expected := []string{"https://scontent.cdninstagram.com/photo.jpg"}
	if !reflect.DeepEqual(urls, expected) {
		t.Fatalf("unexpected Instagram image URLs:\nwant: %#v\n got: %#v", expected, urls)
	}
	if caption != "Photo caption from Instagram" {
		t.Fatalf("unexpected Instagram photo caption: %q", caption)
	}
}

func TestInstagramPageMediaDoesNotReturnVideoCover(t *testing.T) {
	cover := `<meta property="og:image" content="https://scontent.cdninstagram.com/cover.jpg">
		<script>{"display_url":"https://scontent.cdninstagram.com/cover.jpg"}</script>`
	for _, test := range []struct {
		name        string
		page        string
		allowImages bool
	}{
		{"reel or TV", cover, false},
		{"video post", `<meta property="og:type" content="instapp:video">` + cover, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			urls, _ := instagramPageMedia(test.page, test.allowImages)
			if len(urls) != 0 {
				t.Fatalf("video cover must allow the video downloader fallback, got %v", urls)
			}
		})
	}
}

func TestCollectApifyMediaUsesResultEntriesAndSkipsCover(t *testing.T) {
	item := map[string]any{
		"thumb": "https://snapcdn.app/cover.jpg",
		"result": []any{
			map[string]any{"url": "https://snapcdn.app/photo-one.jpg"},
			map[string]any{"url": "https://snapcdn.app/photo-two.jpg"},
		},
	}
	expected := []string{
		"https://snapcdn.app/photo-one.jpg",
		"https://snapcdn.app/photo-two.jpg",
	}
	if actual := collectApifyMedia(item); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("unexpected Apify URLs:\nwant: %#v\n got: %#v", expected, actual)
	}
}

func TestInstagramApifyInputUsesActorURLField(t *testing.T) {
	data, err := json.Marshal(instagramApifyInput{
		URL: []string{"https://www.instagram.com/p/ABC123/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	if !strings.Contains(encoded, `"url"`) || strings.Contains(encoded, "directUrls") {
		t.Fatalf("unexpected Apify input: %s", encoded)
	}
}

func TestInstagramMediaHostsIncludeApifyProxy(t *testing.T) {
	for _, host := range []string{
		"scontent.cdninstagram.com",
		"instagram.fala.snapcdn.app",
		"api.apify.com",
	} {
		if !instagramMediaHost(host) {
			t.Errorf("expected trusted Instagram media host: %s", host)
		}
	}
	if instagramMediaHost("instagram.example.com") {
		t.Fatal("unrelated host must not be trusted")
	}
}

func TestInstagramVXFallback(t *testing.T) {
	for _, host := range []string{"vxinstagram.com", "d.rapidcdn.app", "dl.snapcdn.app", "example.com"} {
		if !isPublicURL(&url.URL{Scheme: "https", Host: host}) {
			t.Skipf("public DNS unavailable for %s; HTTP requests in this test are mocked", host)
		}
	}

	for _, test := range []struct {
		name, path, location, contentType, body, wantErr string
		status                                           int
		cancel, noApify                                  bool
	}{
		{name: "Reel", path: "reel", contentType: "video/mp4", body: "mp4", status: 200},
		{name: "Reels redirect", path: "reels", location: "https://d.rapidcdn.app/clip", contentType: "video/mp4", body: "mp4", status: 200},
		{name: "TV redirect", path: "tv", location: "https://dl.snapcdn.app/get?token=fake", contentType: "video/mp4", body: "mp4", status: 200},
		{name: "cover", path: "reel", contentType: "image/jpeg", body: "jpg", status: 200, wantErr: "expected video"},
		{name: "HTML at MP4 URL", path: "reel", location: "https://d.rapidcdn.app/clip.mp4", contentType: "text/html", body: "html", status: 200, wantErr: "unsupported media content type"},
		{name: "service error", path: "reel", status: 502, wantErr: "media HTTP 502"},
		{name: "service error without paid fallback", path: "reel", status: 502, noApify: true, wantErr: "media HTTP 502"},
		{name: "size limit", path: "reel", contentType: "video/mp4", body: "too large", status: 200, wantErr: "exceeds 4 bytes"},
		{name: "offsite redirect", path: "reel", location: "https://example.com/clip.mp4", status: 200, wantErr: "unsupported media host"},
		{name: "insecure redirect", path: "reel", location: "http://d.rapidcdn.app/clip.mp4", status: 200, wantErr: "unsupported media host"},
		{name: "private redirect", path: "reel", location: "https://127.0.0.1/clip.mp4", status: 200, wantErr: "non-public"},
		{name: "carousel skips single-item fallback", path: "p", wantErr: "Apify HTTP 503"},
		{name: "cancellation", path: "reel", cancel: true, wantErr: "context canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			vxRequests, mediaRequests, apifyRequests := 0, 0, 0
			d := New(config.Config{YTDownloadPath: filepath.Join(t.TempDir(), "missing-yt-dlp"), MaxFileSize: 4, MaxMediaItems: 10, APIFYToken: "fake"})
			if test.noApify {
				d.cfg.APIFYToken = ""
			}
			d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: 200, Header: make(http.Header), Request: request, ContentLength: -1}
				body := ""
				switch request.URL.Hostname() {
				case "www.instagram.com":
					body = `<meta property="og:description" content="A reel caption">`
				case "vxinstagram.com":
					vxRequests++
					if request.URL.Path != "/offload/ABC123" {
						t.Fatalf("unexpected offload path: %s", request.URL.Path)
					}
					if test.cancel {
						cancel()
						return nil, ctx.Err()
					}
					if test.location != "" {
						response.StatusCode = 302
						response.Header.Set("Location", test.location)
						break
					}
					response.StatusCode = test.status
					response.Header.Set("Content-Type", test.contentType)
					body = test.body
				case "d.rapidcdn.app", "dl.snapcdn.app":
					mediaRequests++
					response.Header.Set("Content-Type", test.contentType)
					body = test.body
				case "api.apify.com":
					apifyRequests++
					response.StatusCode = 503
				default:
					t.Fatalf("connected to unexpected host: %s", request.URL.Host)
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return response, nil
			})
			value, _ := url.Parse("https://www.instagram.com/" + test.path + "/ABC123/")
			result, err := d.downloadInstagram(ctx, value)
			if test.wantErr == "" {
				if err != nil || len(result.Items) != 1 || result.Items[0].Kind != "video" || string(result.Items[0].Data) != "mp4" || result.Source != "instagram" || result.Caption != "A reel caption" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if apifyRequests != 0 {
					t.Fatal("successful fallback must not invoke Apify")
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("want error containing %q, got %v", test.wantErr, err)
			} else if test.cancel {
				if !errors.Is(err, context.Canceled) || apifyRequests != 0 {
					t.Fatalf("cancellation must stop fallback: Apify requests=%d err=%v", apifyRequests, err)
				}
			} else if test.noApify {
				if apifyRequests != 0 || strings.Contains(err.Error(), "APIFY_TOKEN") {
					t.Fatalf("unconfigured paid fallback must not be required: requests=%d err=%v", apifyRequests, err)
				}
			} else if apifyRequests != 1 {
				t.Fatalf("failed fallback must continue to Apify, requests=%d", apifyRequests)
			}
			if test.path == "p" {
				if vxRequests != 0 {
					t.Fatal("carousel must not use a single-video fallback")
				}
			} else if vxRequests != 1 {
				t.Fatalf("expected one vxinstagram request, got %d", vxRequests)
			}
			if strings.Contains(test.name, "redirect") && test.wantErr != "" && mediaRequests != 0 {
				t.Fatal("unsupported redirect must be rejected before connecting")
			}
		})
	}
}
