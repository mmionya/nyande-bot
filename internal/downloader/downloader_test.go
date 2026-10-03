package downloader

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestDownloadRetriesOnlyTemporaryFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		cancelled bool
		wantCalls int
	}{
		{"forbidden", http.StatusForbidden, false, 1},
		{"not found", http.StatusNotFound, false, 1},
		{"rate limited", http.StatusTooManyRequests, false, 3},
		{"server error", http.StatusServiceUnavailable, false, 3},
		{"cancelled", http.StatusServiceUnavailable, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := New(config.Config{RetryAttempts: 3})
			d.client.Transport = mediaRedirectTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			_, err := d.Download(ctx, "https://8.8.8.8/video.mp4")
			if err == nil || calls != tc.wantCalls {
				t.Fatalf("Download() calls = %d, err = %v; want %d calls and an error", calls, err, tc.wantCalls)
			}
		})
	}
}

func TestRetryableYTDLPFailures(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"yt-dlp failed: exit status 1: ERROR: unable to download video data: HTTP Error 403: Forbidden", true},
		{"ERROR: Unable to download webpage: HTTP Error 403: Forbidden", false},
		{"ERROR: Unable to download API page: HTTP Error 403: Forbidden", false},
		{"ERROR: Sign in to confirm you're not a bot", false},
		{"ERROR: unable to download video data: HTTP Error 404: Not Found", false},
		{"ERROR: Unable to download webpage: HTTP Error 429: Too Many Requests", true},
		{"ERROR: unable to download video data: HTTP Error 503: Service Unavailable", true},
		{"ERROR: Requested format is not available", false},
	} {
		if got := retryable(errors.New(tc.message)); got != tc.want {
			t.Errorf("retryable(%q) = %v; want %v", tc.message, got, tc.want)
		}
	}
}

func TestSupportedPlatformsAndDirectMedia(t *testing.T) {
	valid := []string{
		"https://www.tiktok.com/@cat/video/123",
		"https://www.instagram.com/reel/ABC123/",
		"https://x.com/user/status/123",
		"https://www.reddit.com/r/cats/comments/abc/title/",
		"https://www.xiaohongshu.com/explore/6411cf99000000001300b6d9",
		"https://xhslink.com/a/qsoHVeD0Liw1",
		"https://www.pinterest.com/pin/664281013778109217/",
		"https://pin.it/4AbCdEf",
		"https://youtu.be/abcdefghijk",
		"https://cdn.example.com/cat.png",
	}
	for _, value := range valid {
		if !Supported(value) {
			t.Errorf("expected supported URL: %s", value)
		}
	}
	if Supported("https://example.com/article") {
		t.Fatal("ordinary article URL must not be treated as downloadable media")
	}
	if Supported("https://xiaohongshu.com.example.com/explore/6411cf99000000001300b6d9") {
		t.Fatal("lookalike Xiaohongshu host must not be supported")
	}
	if Supported("https://pinterest.com.example.com/pin/664281013778109217/") {
		t.Fatal("lookalike Pinterest host must not be supported")
	}
}

func TestCaptionTruncatesByRunes(t *testing.T) {
	value := truncateCaption(string(make([]rune, 0)))
	if value != "" {
		t.Fatalf("unexpected empty caption result: %q", value)
	}
	long := ""
	for range 1100 {
		long += "я"
	}
	result := truncateCaption(long)
	if len([]rune(result)) != 1024 {
		t.Fatalf("expected 1024 runes, got %d", len([]rune(result)))
	}
}

func TestYouTubeYTDLPOptionsAlwaysProduceMP4(t *testing.T) {
	value, err := url.Parse("https://www.youtube.com/watch?v=abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	options := ytdlpMediaOptions(value)
	if !slices.Contains(options, "--recode-video") {
		t.Fatal("YouTube options must force final conversion to MP4")
	}
	if !containsAdjacent(options, "--recode-video", "mp4") {
		t.Fatalf("expected --recode-video mp4, got %#v", options)
	}
	if !containsAdjacent(options, "--js-runtimes", "deno") {
		t.Fatalf("expected the Deno JavaScript runtime for YouTube, got %#v", options)
	}
	formatIndex := slices.Index(options, "--format")
	if formatIndex < 0 || formatIndex+1 >= len(options) {
		t.Fatalf("missing format selection in %#v", options)
	}
	format := options[formatIndex+1]
	if !strings.Contains(format, "[ext=mp4]") || !strings.Contains(format, "[ext=m4a]") {
		t.Fatalf("YouTube format must prefer MP4 video and M4A audio, got %q", format)
	}
	av1Index := strings.Index(format, "[vcodec^=av01]")
	hevcIndex := strings.Index(format, "[vcodec^=hev1]")
	h264Index := strings.Index(format, "[vcodec^=avc1]")
	if av1Index < 0 || hevcIndex < 0 || h264Index < 0 || !(av1Index < hevcIndex && hevcIndex < h264Index) {
		t.Fatalf("expected codec preference AV1, HEVC, H.264, got %q", format)
	}
}

func TestYouTubeYTDLPFormatsFallBackToLowerResolutions(t *testing.T) {
	value, err := url.Parse("https://www.youtube.com/watch?v=abcdefghijk")
	if err != nil {
		t.Fatal(err)
	}
	formats := ytdlpMediaFormats(value)
	wantHeights := []string{"1080", "720", "480", "360", "240", "144"}
	if len(formats) != len(wantHeights) {
		t.Fatalf("expected %d YouTube format attempts, got %d", len(wantHeights), len(formats))
	}
	for index, height := range wantHeights {
		if !strings.Contains(formats[index], "[height<="+height+"]") {
			t.Fatalf("format attempt %d must be limited to %sp: %q", index, height, formats[index])
		}
	}
}

// Exercise yt-dlp's actual selector offline: /best used to bypass the height
// limit, retrying the same oversized video at every lower resolution.
func TestYouTubeFormatDoesNotExceedHeight(t *testing.T) {
	ytdlp, err := exec.LookPath("yt-dlp")
	if err != nil {
		t.Skip("yt-dlp is not installed")
	}
	infoPath := filepath.Join(t.TempDir(), "video.info.json")
	info := `{"id":"clip","title":"clip","extractor":"test","webpage_url":"https://example.com/clip","formats":[
		{"format_id":"high","url":"https://example.com/high.mp4","ext":"mp4","height":1080,"vcodec":"avc1","acodec":"mp4a"}]}`
	if err := os.WriteFile(infoPath, []byte(info), 0600); err != nil {
		t.Fatal(err)
	}
	for _, height := range []int{1080, 720} {
		output, err := exec.Command(ytdlp, "--ignore-config", "--simulate", "--no-warnings",
			"--load-info-json", infoPath, "--format", youtubeMediaFormat(height), "--print", "format_id").CombinedOutput()
		if height == 1080 {
			if err != nil || strings.TrimSpace(string(output)) != "high" {
				t.Fatalf("1080p format must remain available: %s, %v", output, err)
			}
		} else if err == nil || !strings.Contains(string(output), "Requested format is not available") {
			t.Fatalf("720p limit must reject 1080p: %s, %v", output, err)
		}
	}
}

func TestYouTubeFormatFallbackOnlyHandlesFormatAndSizeFailures(t *testing.T) {
	for _, err := range []error{
		errYTDLPNoMedia,
		errYTDLPMediaTooLarge,
		errors.New("yt-dlp failed: requested format is not available"),
	} {
		if !youtubeFormatFallbackError(err) {
			t.Fatalf("expected fallback for %v", err)
		}
	}
	if youtubeFormatFallbackError(errors.New("yt-dlp failed: video unavailable")) {
		t.Fatal("permanent source errors must not trigger every format fallback")
	}
}

func TestYTDLPSizeLimitOutput(t *testing.T) {
	if !ytdlpSizeLimitOutput("File is larger than max-filesize. Aborting") {
		t.Fatal("expected yt-dlp max-filesize output to be recognized")
	}
	if ytdlpSizeLimitOutput("HTTP Error 403: Forbidden") {
		t.Fatal("unrelated errors must not be classified as size limits")
	}
}

func TestNonYouTubeYTDLPOptionsDoNotForceReencode(t *testing.T) {
	value, err := url.Parse("https://www.reddit.com/r/cats/comments/example")
	if err != nil {
		t.Fatal(err)
	}
	options := ytdlpMediaOptions(value)
	if slices.Contains(options, "--recode-video") {
		t.Fatalf("non-YouTube downloads must not be needlessly re-encoded: %#v", options)
	}
}

func TestYouTubeCaptionContainsOnlyTitle(t *testing.T) {
	info := map[string]any{
		"title":       "  Video title  ",
		"description": "Long video description",
		"uploader":    "Channel name",
	}
	if caption := ytdlpCaptionFromInfo(info, true); caption != "Video title" {
		t.Fatalf("expected only the YouTube title, got %q", caption)
	}
}

func containsAdjacent(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}

func TestFilenameFromURLAddsMissingMediaExtension(t *testing.T) {
	for _, tc := range []struct{ name, path, mime, want string }{
		{"extensionless CDN video", "/video/tos/oYBZJKBER", "video/mp4", "oYBZJKBER.mp4"},
		{"MP4 with parameters", "/opaque-id", "video/mp4; charset=binary", "opaque-id.mp4"},
		{"empty path", "", "video/mp4", "media.mp4"},
		{"root path", "/", "video/mp4", "media.mp4"},
		{"existing extension", "/clip.mp4", "video/mp4", "clip.mp4"},
		{"preserve existing video format", "/clip.webm", "video/webm", "clip.webm"},
		{"extensionless WebM", "/clip", "video/webm", "clip.webm"},
		{"extensionless image", "/photo", "image/png", "photo.png"},
		{"unknown type", "/download", "application/x-nyande-unknown", "download.bin"},
		{"long name", "/" + strings.Repeat("v", 150), "video/mp4", strings.Repeat("v", 100) + ".mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := filenameFromURL(&url.URL{Path: tc.path}, tc.mime); got != tc.want {
				t.Fatalf("filenameFromURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
