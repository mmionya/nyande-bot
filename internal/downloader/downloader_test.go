package downloader

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
)

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
