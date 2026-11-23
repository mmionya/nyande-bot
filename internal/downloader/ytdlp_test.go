package downloader

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestYouTubeStopsUselessQualityRetries(t *testing.T) {
	for _, test := range []struct {
		name, first, later  string
		wantCalls           int
		wantError, tooLarge bool
	}{
		{"unavailable", "unavailable", "unavailable", 1, true, false},
		{"empty", "empty", "empty", 1, true, false},
		{"network", "network", "network", 1, true, false},
		{"lower quality succeeds", "size", "success", 2, false, false},
		{"no smaller format", "size", "unavailable", 2, true, true},
		{"smaller format empty", "size", "empty", 2, true, true},
		{"all qualities too large", "size", "size", 6, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := scriptedYTDLP(t, test.first, test.later)
			d := New(config.Config{YTDownloadPath: script, MaxFileSize: 1024, MaxMediaItems: 1})
			value, _ := url.Parse("https://www.youtube.com/watch?v=abcdefghijk")
			result, err := d.downloadYTDLP(context.Background(), value)
			calls, readErr := os.ReadFile(script + ".calls")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if count := strings.Count(string(calls), "\n"); count != test.wantCalls {
				t.Fatalf("extractor calls=%d, want %d; err=%v", count, test.wantCalls, err)
			}
			if (err != nil) != test.wantError || errors.Is(err, errYTDLPMediaTooLarge) != test.tooLarge {
				t.Fatalf("err=%v; wantError=%t tooLarge=%t", err, test.wantError, test.tooLarge)
			}
			if !test.wantError && (len(result.Items) != 1 || result.Source != "youtube") {
				t.Fatalf("unexpected successful download: %+v", result)
			}
		})
	}
}

// The fake records the format and extractor args on each call. Its last reply
// repeats, so an unexpected retry can't turn a terminal failure into success.
func scriptedYTDLP(t *testing.T, replies ...string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "yt-dlp")
	contents := `#!/bin/sh
set -eu
output=''
format=''
extractor_args=''
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output="$2"; shift ;;
        --format) format="$2"; shift ;;
        --extractor-args) extractor_args="$2"; shift ;;
    esac
    shift
done
printf '%s\t%s\n' "$format" "$extractor_args" >> "$0.calls"
call=$(wc -l < "$0.calls")
set -- ` + strings.Join(replies, " ") + `
while [ "$call" -gt 1 ] && [ "$#" -gt 1 ]; do
    call=$((call - 1))
    shift
done
provider_error="Command '['deno', 'run', '/root/bgutil-ytdlp-pot-provider/server/src/generate_once.ts', '--version']' timed out after 15.0 seconds"
case "$1" in
    provider) echo "ERROR: $provider_error"; exit 1 ;;
    warning) echo "WARNING: $provider_error"; echo 'ERROR: HTTP Error 403: Forbidden'; exit 1 ;;
    unavailable) echo 'ERROR: Requested format is not available'; exit 1 ;;
    network) echo 'ERROR: HTTP Error 503: Service Unavailable'; exit 1 ;;
    size) echo 'File is larger than max-filesize. Aborting'; exit 0 ;;
    empty) exit 0 ;;
    success) printf mp4 > "${output%/*}/clip.mp4" ;;
    *) exit 2 ;;
esac
`
	if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestYouTubeShortsRecoverFromProviderTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies []string
		limits  []int
		wantErr string
	}{
		{"same quality succeeds", []string{"provider", "success"}, []int{1080, 1080}, ""},
		{"smaller format after recovery", []string{"provider", "size", "success"}, []int{1080, 1080, 720}, ""},
		{"warning with unrelated fatal error", []string{"warning"}, []int{1080}, "HTTP Error 403"},
		{"fallback failure", []string{"provider", "unavailable"}, []int{1080, 1080}, "Requested format is not available"},
		{"repeated provider failure", []string{"provider", "provider"}, []int{1080, 1080}, "timed out after 15.0 seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := scriptedYTDLP(t, tc.replies...)
			d := New(config.Config{YTDownloadPath: script, MaxFileSize: 1024, MaxMediaItems: 1, RetryAttempts: 3})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := d.Download(ctx, "https://www.youtube.com/shorts/abcdefghijk?si=test")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want failure containing %q", err, tc.wantErr)
				}
			} else if err != nil || result.Source != "youtube" || len(result.Items) != 1 || string(result.Items[0].Data) != "mp4" {
				t.Fatalf("expected a downloaded YouTube video, got %+v, %v", result, err)
			}
			data, err := os.ReadFile(script + ".calls")
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(calls) != len(tc.limits) {
				t.Fatalf("got %d extractor calls, want %d", len(calls), len(tc.limits))
			}
			for i, limit := range tc.limits {
				format, args, ok := strings.Cut(calls[i], "\t")
				wantArgs := ""
				if i > 0 {
					wantArgs = "youtube:fetch_pot=never"
				}
				if !ok || format != youtubeMediaFormat(limit) || args != wantArgs {
					t.Errorf("call %d: got format %q, extractor args %q; want %dp, %q", i+1, format, args, limit, wantArgs)
				}
			}
		})
	}
}
