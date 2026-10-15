package downloader

import (
	"context"
	"errors"
	"fmt"
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

func TestNativeCompressionFailureSkipsExtractorFallback(t *testing.T) {
	for _, source := range []string{"pinterest", "xiaohongshu"} {
		t.Run(source, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "yt-dlp")
			if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$0.called\"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			d := New(config.Config{YTDownloadPath: script, MaxFileSize: 1024})
			fallback := d.downloadPinterestFallback
			if source == "xiaohongshu" {
				fallback = d.downloadXiaohongshuFallback
			}
			value, _ := url.Parse("https://www." + source + ".com/post/123/")
			failure := fmt.Errorf("%w: timeout", ErrVideoCompression)
			_, err := fallback(context.Background(), value, failure)
			if !errors.Is(err, failure) {
				t.Fatalf("compression error was lost: %v", err)
			}
			if _, err := os.Stat(script + ".called"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("extractor was invoked after compression failed: %v", err)
			}
		})
	}
}

func TestPinterestShortLinkSkipsUnusedPinPage(t *testing.T) {
	const pinURL = "https://www.pinterest.com/pin/123/"
	for _, intermediate := range []string{"", "https://api.pinterest.com/url_shortener/example/redirect/"} {
		t.Run(intermediate, func(t *testing.T) {
			d := New(config.Config{})
			requests := 0
			d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.String() == pinURL {
					t.Fatal("pin HTML must not be requested before using the API")
				}
				target := pinURL
				if requests == 1 && intermediate != "" {
					target = intermediate
				}
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
			})
			shortURL, _ := url.Parse("https://pin.it/example")
			resolved, err := d.resolvePinterestURL(context.Background(), shortURL)
			wantRequests := 1
			if intermediate != "" {
				wantRequests++
			}
			if err != nil || resolved.String() != pinURL || requests != wantRequests {
				t.Fatalf("resolved=%v err=%v requests=%d, want %s in %d requests", resolved, err, requests, pinURL, wantRequests)
			}
		})
	}
}

func TestPinterestShortLinkPreservesFailures(t *testing.T) {
	policyErr := errors.New("redirect policy rejected target")
	for _, test := range []struct {
		name   string
		target string
		status int
		policy bool
	}{
		{name: "HTTP failure", status: http.StatusForbidden},
		{name: "outside Pinterest", status: http.StatusFound, target: "https://example.com/pin/123/"},
		{name: "redirect policy", status: http.StatusFound, target: "https://www.pinterest.com/pin/123/", policy: true},
		{name: "redirect limit", status: http.StatusFound, target: "https://pin.it/loop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := New(config.Config{})
			if test.policy {
				d.client.CheckRedirect = func(*http.Request, []*http.Request) error { return policyErr }
			}
			requests := 0
			d.client.Transport = mediaRedirectTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				status := test.status
				if request.URL.Hostname() == "example.com" {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {test.target}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
			})
			shortURL, _ := url.Parse("https://pin.it/example")
			_, err := d.resolvePinterestURL(context.Background(), shortURL)
			if err == nil {
				t.Fatal("expected redirect or HTTP failure")
			}
			if test.policy && (!errors.Is(err, policyErr) || requests != 1) {
				t.Fatalf("policy must run before accepting a pin: err=%v requests=%d", err, requests)
			}
			if requests > 6 {
				t.Fatalf("redirect limit was ignored: %d requests", requests)
			}
		})
	}
}

func TestPinterestPinFromResponseAndImage(t *testing.T) {
	pin, err := pinterestPinFromResponse([]byte(`{
		"resource_response":{"data":{
			"id":"123", "title":"Cat room", "description":"A cozy room",
			"closeup_attribution":{"full_name":"Mimi"},
			"images":{
				"236x":{"url":"https://i.pinimg.com/236x/cat.jpg","width":236,"height":236},
				"orig":{"url":"https://i.pinimg.com/originals/cat.jpg","width":1200,"height":1600},
				"evil":{"url":"https://example.com/cat.jpg","width":9999,"height":9999}
			}
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	candidates := pinterestMediaCandidates(pin)
	want := []pinterestMediaCandidate{{kind: "photo", urls: []string{
		"https://i.pinimg.com/originals/cat.jpg",
		"https://i.pinimg.com/236x/cat.jpg",
	}}}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("unexpected Pinterest candidates:\nwant: %#v\n got: %#v", want, candidates)
	}
	if caption := pinterestCaption(pin); caption != "Cat room\n\nA cozy room\n\nMimi" {
		t.Fatalf("unexpected Pinterest caption: %q", caption)
	}
}

func TestPinterestCarouselPreservesMixedMediaOrder(t *testing.T) {
	pin := map[string]any{
		"carousel_data": map[string]any{"carousel_slots": []any{
			map[string]any{"images": map[string]any{
				"orig": map[string]any{"url": "https://i.pinimg.com/originals/one.jpg", "width": float64(1000), "height": float64(1500)},
			}},
			map[string]any{"videos": map[string]any{"video_list": map[string]any{
				"V_EXP7": map[string]any{"url": "https://v.pinimg.com/videos/small.mp4", "width": float64(360), "height": float64(640)},
				"V_HLS":  map[string]any{"url": "https://v.pinimg.com/videos/video.m3u8", "width": float64(1080), "height": float64(1920)},
				"V_720P": map[string]any{"url": "https://v.pinimg.com/videos/large.mp4", "width": float64(720), "height": float64(1280)},
			}}},
		}},
	}
	want := []pinterestMediaCandidate{
		{kind: "photo", urls: []string{"https://i.pinimg.com/originals/one.jpg"}},
		{kind: "video", urls: []string{
			"https://v.pinimg.com/videos/large.mp4",
			"https://v.pinimg.com/videos/small.mp4",
		}},
	}
	if got := pinterestMediaCandidates(pin); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected Pinterest carousel:\nwant: %#v\n got: %#v", want, got)
	}
}

func TestPinterestPinFromResponseRequiresData(t *testing.T) {
	if _, err := pinterestPinFromResponse([]byte(`{"resource_response":{"data":null}}`)); err == nil {
		t.Fatal("expected missing pin data to fail")
	}
}

func TestPinterestHLSVideoDoesNotFallBackToCoverImage(t *testing.T) {
	pin := map[string]any{
		"videos": map[string]any{"video_list": map[string]any{
			"V_HLS": map[string]any{"url": "https://v.pinimg.com/videos/video.m3u8"},
		}},
		"images": map[string]any{
			"orig": map[string]any{"url": "https://i.pinimg.com/originals/cover.jpg"},
		},
	}
	want := []pinterestMediaCandidate{{kind: "video"}}
	if got := pinterestMediaCandidates(pin); !reflect.DeepEqual(got, want) {
		t.Fatalf("HLS video cover must not be returned as the pin media: %#v", got)
	}
}
