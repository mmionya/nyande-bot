package bot

import (
	"path/filepath"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

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
	if _, added, err := list.Add("example.com"); err != nil || !added {
		t.Fatalf("add failed: added=%v err=%v", added, err)
	}
	reloaded, err := loadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Allows("https://media.example.com/post") {
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
