package bot

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func TestYouTubeDownloadErrorMessages(t *testing.T) {
	const short = "https://youtube.com/shorts/abcdefghijk?si=shared"
	for _, test := range []struct {
		url, diagnostic, key string
	}{
		{short, "ERROR: [youtube] abc: Requested format is not available.", "downloader.error.youtube_format"},
		{short, "ERROR: [youtube] abc: Sign in to confirm you’re not a bot", "downloader.error.youtube_verification"},
		{short, "ERROR: [youtube] abc: Private video. Sign in if you've been granted access to this video", "downloader.error.youtube_unavailable"},
		{short, "ERROR: [youtube] abc: Video unavailable", "downloader.error.youtube_unavailable"},
		{short, "ERROR: Unable to download webpage: HTTP Error 429: Too Many Requests", "downloader.error.youtube_rate_limit"},
		{short, "ERROR: unable to download video data: HTTP Error 403: Forbidden", "downloader.error.youtube_forbidden"},
		{short, "WARNING: private video\nERROR: unknown failure", "downloader.error.generic"},
		{"https://example.com/item.mp4", "ERROR: Requested format is not available.", "downloader.error.generic"},
	} {
		t.Run(test.diagnostic, func(t *testing.T) {
			err := errors.New("yt-dlp failed: exit status 1: " + test.diagnostic + " (sensitive details)")
			if got, want := humanDownloadError(test.url, err), resources.Get(test.key); got != want {
				t.Errorf("humanDownloadError() = %q, want %q", got, want)
			}
		})
	}
}

func TestExtractURLsUsesTelegramUTF16Offsets(t *testing.T) {
	text := "🐈 смотри https://example.com/cat.jpg"
	message := &telegram.Message{
		Text: text,
		Entities: []telegram.MessageEntity{{
			Type: "url", Offset: 10, Length: 27,
		}},
	}
	urls := extractURLs(message)
	if len(urls) != 1 || urls[0] != "https://example.com/cat.jpg" {
		t.Fatalf("unexpected URLs: %#v", urls)
	}
}

func TestAllowlistPersistsSubdomainRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowed.json")
	list, err := loadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, added, err := list.Add(-1, "example.com"); err != nil || !added {
		t.Fatalf("add failed: added=%v err=%v", added, err)
	}
	reloaded, err := loadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Allows(-1, "https://media.example.com/post") {
		t.Fatal("persisted parent domain should allow a subdomain")
	}
}

func TestLinkDeletionDisabledByDefault(t *testing.T) {
	b := &Bot{}
	if b.linkDeletionEnabled(-1001) {
		t.Fatal("link deletion should be disabled when linkConfig is nil")
	}

	path := filepath.Join(t.TempDir(), "link-moderation.json")
	settings, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	b.linkConfig = settings
	if b.linkDeletionEnabled(-1001) {
		t.Fatal("link deletion should be disabled by default for unconfigured chats")
	}
	if err := b.linkConfig.Set(-1001, true); err != nil {
		t.Fatal(err)
	}
	if !b.linkDeletionEnabled(-1001) {
		t.Fatal("link deletion should be enabled after explicit configuration")
	}
}
