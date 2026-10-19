package bot

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLLMChatSettingsPersistenceAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "llm.json")
	s, err := loadLLMChatSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var absent *llmChatSettings
	if absent.Disabled(-10) || s.Disabled(-10) || s.Disabled(10) {
		t.Fatal("new chats must use the enabled default")
	}
	if err := s.SetDisabled(-10, true); err != nil {
		t.Fatal(err)
	}
	if !s.Disabled(-10) || s.Disabled(-20) || s.Disabled(10) {
		t.Fatal("disabling one chat changed another chat")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings file permissions: %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("settings directory permissions: %v, %v", info, err)
	}
	s, err = loadLLMChatSettings(path)
	if err != nil || !s.Disabled(-10) || s.Disabled(-20) {
		t.Fatalf("disabled setting did not survive reload: %v", err)
	}
	if err := s.SetDisabled(-20, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(-10, false); err != nil {
		t.Fatal(err)
	}
	s, err = loadLLMChatSettings(path)
	if err != nil || s.Disabled(-10) || !s.Disabled(-20) {
		t.Fatalf("reenabled setting did not survive reload: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored storedLLMChatSettings
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.DisabledChats) != 1 || stored.DisabledChats[0] != -20 {
		t.Fatalf("expected only disabled chats on disk: %s", data)
	}
	if err := s.SetDisabled(-20, false); err != nil {
		t.Fatal(err)
	}
	s, err = loadLLMChatSettings(path)
	if err != nil || s.Disabled(-20) {
		t.Fatalf("all-enabled setting did not survive reload: %v", err)
	}
}

func TestLLMChatSettingsRejectsInvalidFile(t *testing.T) {
	for name, data := range map[string]string{
		"empty": "", "invalid": `{`, "null": `null`, "array": `[]`,
		"wrong type":     `{"disabled_chats":["-10"]}`,
		"unknown field":  `{"disable_chats":[-10]}`,
		"trailing value": `{"disabled_chats":[]} {}`,
		"trailing junk":  `{"disabled_chats":[]} unexpected`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "llm.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadLLMChatSettings(path); err == nil {
				t.Fatal("invalid file silently used enabled defaults")
			}
		})
	}
	if _, err := loadLLMChatSettings(t.TempDir()); err == nil {
		t.Fatal("read failure silently used enabled defaults")
	}
}

func TestLLMChatSettingsFailedWriteKeepsPreviousState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.json")
	s, err := loadLLMChatSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(-10, true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "llm.json")
	if err := s.SetDisabled(-10, false); err == nil {
		t.Fatal("expected save error")
	}
	if err := s.SetDisabled(-20, true); err == nil {
		t.Fatal("expected save error")
	}
	if !s.Disabled(-10) || s.Disabled(-20) {
		t.Fatal("failed save changed the live state")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed save changed previous settings on disk: %v", err)
	}
	reloaded, err := loadLLMChatSettings(path)
	if err != nil || !reloaded.Disabled(-10) || reloaded.Disabled(-20) {
		t.Fatalf("failed save changed persisted state: %v", err)
	}
}

func TestLLMChatSettingsFailedRenameCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "llm.json")
	s, err := loadLLMChatSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(-10, true); err == nil {
		t.Fatal("expected rename error")
	}
	if s.Disabled(-10) {
		t.Fatal("failed rename changed the live state")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "llm.json" || !entries[0].IsDir() {
		t.Fatalf("temporary settings file was left behind: %v", entries)
	}
}

func TestLLMChatSettingsConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm.json")
	s, err := loadLLMChatSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := int64(1); i <= 12; i++ {
		workers.Add(1)
		go func(chatID int64) {
			defer workers.Done()
			for j := 0; j < 5; j++ {
				if err := s.SetDisabled(chatID, true); err != nil {
					t.Error(err)
					return
				}
				if !s.Disabled(chatID) {
					t.Errorf("chat %d lost its setting", chatID)
				}
				if err := s.SetDisabled(chatID, false); err != nil {
					t.Error(err)
					return
				}
			}
			if err := s.SetDisabled(chatID, chatID%2 == 0); err != nil {
				t.Error(err)
			}
		}(i)
	}
	workers.Wait()
	reloaded, err := loadLLMChatSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 12; i++ {
		if want := i%2 == 0; s.Disabled(i) != want || reloaded.Disabled(i) != want {
			t.Errorf("chat %d: live=%t persisted=%t want=%t", i, s.Disabled(i), reloaded.Disabled(i), want)
		}
	}
}
