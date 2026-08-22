package bot

import (
	"path/filepath"
	"testing"
)

func TestLinkDeletionSettingsPersistPerChat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "link-moderation.json")
	settings, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled(-1001) {
		t.Fatal("link deletion should be enabled by default")
	}
	if err := settings.Set(-1001, false); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(-1002, false); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Enabled(-1001) || reloaded.Enabled(-1002) {
		t.Fatal("disabled chats were not restored")
	}
	if !reloaded.Enabled(-1003) {
		t.Fatal("an unrelated chat should keep the default setting")
	}

	if err := reloaded.Set(-1001, true); err != nil {
		t.Fatal(err)
	}
	restarted, err := loadLinkDeletionSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Enabled(-1001) || restarted.Enabled(-1002) {
		t.Fatal("updated per-chat settings were not persisted")
	}
}
