package bot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// llmChatSettings stores only chats which explicitly disabled the LLM. Chats
// absent from the file retain the normal globally configured behavior.
type llmChatSettings struct {
	mu            sync.RWMutex
	path          string
	disabledChats map[int64]struct{}
}

type storedLLMChatSettings struct {
	DisabledChats []int64 `json:"disabled_chats"`
}

func loadLLMChatSettings(path string) (*llmChatSettings, error) {
	settings := &llmChatSettings{path: path, disabledChats: make(map[int64]struct{})}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored *storedLLMChatSettings
	if err := decoder.Decode(&stored); err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, errors.New("LLM chat settings must be a JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values in LLM chat settings")
		}
		return nil, err
	}
	for _, chatID := range stored.DisabledChats {
		settings.disabledChats[chatID] = struct{}{}
	}
	return settings, nil
}

func (s *llmChatSettings) Disabled(chatID int64) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, disabled := s.disabledChats[chatID]
	return disabled
}

func (s *llmChatSettings) SetDisabled(chatID int64, disabled bool) error {
	if s == nil {
		return errors.New("LLM chat settings unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, wasDisabled := s.disabledChats[chatID]
	if wasDisabled == disabled {
		return nil
	}

	// Build and persist the replacement before changing the live state. A failed
	// write therefore leaves both the previous file and in-memory setting intact.
	next := make(map[int64]struct{}, len(s.disabledChats)+1)
	for id := range s.disabledChats {
		next[id] = struct{}{}
	}
	if disabled {
		next[chatID] = struct{}{}
	} else {
		delete(next, chatID)
	}
	chatIDs := make([]int64, 0, len(next))
	for id := range next {
		chatIDs = append(chatIDs, id)
	}
	sort.Slice(chatIDs, func(i, j int) bool { return chatIDs[i] < chatIDs[j] })
	data, err := json.MarshalIndent(storedLLMChatSettings{DisabledChats: chatIDs}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeLLMChatSettings(s.path, append(data, '\n')); err != nil {
		return err
	}
	s.disabledChats = next
	return nil
}

func writeLLMChatSettings(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
