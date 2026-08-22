package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type linkDeletionSettings struct {
	mu            sync.RWMutex
	path          string
	disabledChats map[int64]struct{}
}

func loadLinkDeletionSettings(path string) (*linkDeletionSettings, error) {
	settings := &linkDeletionSettings{path: path, disabledChats: make(map[int64]struct{})}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return nil, err
	}
	var stored struct {
		DisabledChats []int64 `json:"disabled_chats"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	for _, chatID := range stored.DisabledChats {
		settings.disabledChats[chatID] = struct{}{}
	}
	return settings, nil
}

func (s *linkDeletionSettings) Enabled(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, disabled := s.disabledChats[chatID]
	return !disabled
}

func (s *linkDeletionSettings) Set(chatID int64, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, wasDisabled := s.disabledChats[chatID]
	if enabled {
		delete(s.disabledChats, chatID)
	} else {
		s.disabledChats[chatID] = struct{}{}
	}
	if err := s.saveLocked(); err != nil {
		if wasDisabled {
			s.disabledChats[chatID] = struct{}{}
		} else {
			delete(s.disabledChats, chatID)
		}
		return err
	}
	return nil
}

func (s *linkDeletionSettings) saveLocked() error {
	chatIDs := make([]int64, 0, len(s.disabledChats))
	for chatID := range s.disabledChats {
		chatIDs = append(chatIDs, chatID)
	}
	sort.Slice(chatIDs, func(left, right int) bool { return chatIDs[left] < chatIDs[right] })
	data, err := json.MarshalIndent(struct {
		DisabledChats []int64 `json:"disabled_chats"`
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
