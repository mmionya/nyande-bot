package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type linkDeletionSettings struct {
	mu           sync.RWMutex
	path         string
	enabledChats map[int64]struct{}
}

func loadLinkDeletionSettings(path string) (*linkDeletionSettings, error) {
	settings := &linkDeletionSettings{path: path, enabledChats: make(map[int64]struct{})}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return nil, err
	}
	var stored struct {
		EnabledChats []int64 `json:"enabled_chats"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	for _, chatID := range stored.EnabledChats {
		settings.enabledChats[chatID] = struct{}{}
	}
	return settings, nil
}

func (s *linkDeletionSettings) Enabled(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, enabled := s.enabledChats[chatID]
	return enabled
}

func (s *linkDeletionSettings) Set(chatID int64, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, wasEnabled := s.enabledChats[chatID]
	if enabled {
		s.enabledChats[chatID] = struct{}{}
	} else {
		delete(s.enabledChats, chatID)
	}
	if err := s.saveLocked(); err != nil {
		if wasEnabled {
			s.enabledChats[chatID] = struct{}{}
		} else {
			delete(s.enabledChats, chatID)
		}
		return err
	}
	return nil
}

func (s *linkDeletionSettings) saveLocked() error {
	chatIDs := make([]int64, 0, len(s.enabledChats))
	for chatID := range s.enabledChats {
		chatIDs = append(chatIDs, chatID)
	}
	sort.Slice(chatIDs, func(left, right int) bool { return chatIDs[left] < chatIDs[right] })
	data, err := json.MarshalIndent(struct {
		EnabledChats []int64 `json:"enabled_chats"`
	}{chatIDs}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}
