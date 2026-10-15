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
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func downloadCompressionTools(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	probe, encoder := filepath.Join(directory, "ffprobe"), filepath.Join(directory, "ffmpeg")
	for name, contents := range map[string]string{
		probe:   "#!/bin/sh\nprintf '%s' '{\"format\":{\"duration\":\"1\"},\"streams\":[{\"index\":0,\"codec_type\":\"video\",\"width\":320,\"height\":240}]}'\n",
		encoder: "#!/bin/sh\nfor output do :; done\nprintf compressed > \"$output\"\n",
	} {
		if err := os.WriteFile(name, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return probe, encoder
}

func TestHTTPVideoCompressionAndSourceBounds(t *testing.T) {
	for _, test := range []struct {
		name, kind                               string
		size                                     int
		unknown, disabled, wantError, compressed bool
	}{
		{name: "small unchanged", kind: "video", size: 32},
		{name: "large video", kind: "video", size: 128 * 1024, compressed: true},
		{name: "unknown size video", kind: "video", size: 128 * 1024, unknown: true, compressed: true},
		{name: "source cap", kind: "video", size: 256*1024 + 1, wantError: true},
		{name: "unknown source cap", kind: "video", size: 256*1024 + 1, unknown: true, wantError: true},
		{name: "photo cap unchanged", kind: "photo", size: 128 * 1024, wantError: true},
		{name: "disabled", kind: "video", size: 128 * 1024, disabled: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe, encoder := downloadCompressionTools(t)
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			d := New(config.Config{MaxFileSize: 64 * 1024, VideoMaxSourceSize: 256 * 1024, VideoCompressionEnabled: !test.disabled,
				FFprobePath: probe, FFmpegPath: encoder, RetryAttempts: 3, MaxMediaItems: 1})
			body := strings.Repeat("x", test.size)
			requests, notices := 0, 0
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				length := int64(test.size)
				if test.unknown {
					length = -1
				}
				mime := "video/mp4"
				if test.kind == "photo" {
					mime = "image/jpeg"
				}
				return &http.Response{StatusCode: 200, ContentLength: length, Request: r,
					Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx := WithVideoCompressionStatus(context.Background(), func(context.Context) { notices++ })
			result, err := d.Download(ctx, "https://8.8.8.8/video.mp4")
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %v", err, test.wantError)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
			if test.compressed {
				if notices != 1 || len(result.Items) != 1 || string(result.Items[0].Data) != "compressed" || result.Items[0].MIME != "video/mp4" {
					t.Fatalf("compression result = %+v, notices = %d", result, notices)
				}
			} else if notices != 0 {
				t.Fatalf("unexpected compression notice")
			} else if !test.wantError && (len(result.Items) != 1 || string(result.Items[0].Data) != body) {
				t.Fatal("small media changed")
			}
			entries, err := os.ReadDir(temporary)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files remain: %v, %v", entries, err)
			}
		})
	}
}

func TestHTTPCompressionFailureDoesNotRedownload(t *testing.T) {
	d := New(config.Config{MaxFileSize: 64 * 1024, VideoMaxSourceSize: 256 * 1024, VideoCompressionEnabled: true,
		FFprobePath: filepath.Join(t.TempDir(), "missing-ffprobe"), RetryAttempts: 3, MaxMediaItems: 1})
	requests := 0
	d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Request: r, ContentLength: -1,
			Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 128*1024)))}, nil
	})
	_, err := d.Download(context.Background(), "https://8.8.8.8/video.mp4")
	if !errors.Is(err, ErrVideoCompression) || requests != 1 || VideoCompressionMessage(err) == "" {
		t.Fatalf("err = %v, requests = %d", err, requests)
	}
}

func TestYouTubeTriesSmallerDownloadsBeforeCompression(t *testing.T) {
	for _, smaller := range []string{"size", "success", "unavailable", "network"} {
		t.Run(smaller, func(t *testing.T) {
			probe, encoder := downloadCompressionTools(t)
			script := filepath.Join(t.TempDir(), "yt-dlp")
			contents := fmt.Sprintf(`#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output="$2"; shift ;;
        --format) format="$2"; shift ;;
        --max-filesize) limit="$2"; shift ;;
    esac
    shift
done
mode=size
if [ -f "$0.calls" ]; then mode=%s; fi
printf '%%s %%s\n' "$limit" "$format" >> "$0.calls"
if [ "$limit" -gt 65536 ]; then
    head -c 131072 /dev/zero > "${output%%/*}/clip.mp4"
    exit 0
fi
case "$mode" in
    size) echo 'File is larger than max-filesize. Aborting';;
    success) printf small > "${output%%/*}/clip.mp4";;
    unavailable) echo 'ERROR: Requested format is not available'; exit 1;;
    network) echo 'ERROR: HTTP Error 503: Service Unavailable'; exit 1;;
esac
`, smaller)
			if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
				t.Fatal(err)
			}
			d := New(config.Config{YTDownloadPath: script, MaxFileSize: 64 * 1024, VideoMaxSourceSize: 256 * 1024,
				VideoCompressionEnabled: true, FFprobePath: probe, FFmpegPath: encoder, MaxMediaItems: 1})
			value, _ := url.Parse("https://www.youtube.com/watch?v=abcdefghijk")
			result, err := d.downloadYTDLP(context.Background(), value)
			calls, readErr := os.ReadFile(script + ".calls")
			if readErr != nil {
				t.Fatal(readErr)
			}
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			wantCalls := map[string]int{"size": 7, "success": 2, "unavailable": 3, "network": 2}[smaller]
			if len(lines) != wantCalls {
				t.Fatalf("calls = %v, err = %v", lines, err)
			}
			for _, line := range lines[:len(lines)-1] {
				if !strings.HasPrefix(line, "65536 ") {
					t.Fatalf("source cap raised before smaller formats: %s", line)
				}
			}
			if smaller == "network" {
				if err == nil {
					t.Fatal("expected network error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "compressed"
			if smaller == "success" {
				want = "small"
			}
			if len(result.Items) != 1 || string(result.Items[0].Data) != want {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
