package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			directory := t.TempDir()
			script := filepath.Join(directory, "yt-dlp")
			contents := fmt.Sprintf(`#!/bin/sh
set -eu
output=''
format=''
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output="$2"; shift ;;
        --format) format="$2"; shift ;;
    esac
    shift
done
mode='%s'
if [ -f "$0.calls" ]; then mode='%s'; fi
printf '%%s\n' "$format" >> "$0.calls"
case "$mode" in
    unavailable) echo 'ERROR: Requested format is not available'; exit 1 ;;
    network) echo 'ERROR: HTTP Error 503: Service Unavailable'; exit 1 ;;
    size) echo 'File is larger than max-filesize. Aborting'; exit 0 ;;
    empty) exit 0 ;;
    success) printf mp4 > "${output%%/*}/clip.mp4" ;;
esac
`, test.first, test.later)
			if err := os.WriteFile(script, []byte(contents), 0700); err != nil {
				t.Fatal(err)
			}
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
