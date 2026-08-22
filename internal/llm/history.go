package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const historyFileVersion = 1

type persistedHistory struct {
	Version int                     `json:"version"`
	Chats   map[int64][]chatMessage `json:"chats"`
}

func (c *Client) loadHistory() error {
	path := strings.TrimSpace(c.cfg.LLMHistoryFile)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var stored persistedHistory
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if stored.Version != historyFileVersion {
		return fmt.Errorf("unsupported history version %d", stored.Version)
	}
	for chatID, messages := range stored.Chats {
		cleaned := make([]chatMessage, 0, len(messages))
		for _, message := range messages {
			content, ok := message.Content.(string)
			if !ok || content == "" || (message.Role != "user" && message.Role != "assistant") {
				continue
			}
			cleaned = append(cleaned, chatMessage{Role: message.Role, Content: content})
		}
		if maximum := c.cfg.LLMMaxHistory; maximum > 0 && len(cleaned) > maximum {
			cleaned = cleaned[len(cleaned)-maximum:]
		}
		if len(cleaned) > 0 {
			c.history[chatID] = cleaned
		}
	}
	return nil
}

// persistHistoryLocked writes the current history atomically. The caller must
// hold c.mu so concurrent chats cannot overwrite a newer snapshot.
func (c *Client) persistHistoryLocked() error {
	path := strings.TrimSpace(c.cfg.LLMHistoryFile)
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(persistedHistory{
		Version: historyFileVersion,
		Chats:   c.history,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return nil
}
