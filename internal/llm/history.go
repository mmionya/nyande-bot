package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mmionya/nyande-bot/internal/storagecrypt"
)

const historyFileVersion = 1
const encryptedHistoryPrefix = "nyande-llm-history-v2\n"

type persistedHistory struct {
	Version int                     `json:"version"`
	Chats   map[int64][]chatMessage `json:"chats"`
}

func (c *Client) loadHistory() error {
	path := strings.TrimSpace(c.cfg.LLMHistoryFile)
	if path == "" {
		return nil
	}
	cipher, err := storagecrypt.New(c.cfg.StorageEncryptionKey)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	encrypted := bytes.HasPrefix(data, []byte(encryptedHistoryPrefix))
	if encrypted {
		if cipher == nil {
			return errors.New("encrypted LLM history requires the storage encryption key")
		}
		data, err = cipher.Open(data[len(encryptedHistoryPrefix):], []byte(encryptedHistoryPrefix+filepath.Base(path)))
		if err != nil {
			return err
		}
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
	if cipher != nil && !encrypted {
		return c.persistHistoryLocked()
	}
	return nil
}

// persistHistoryLocked writes the current history atomically. The caller must
// hold c.mu so concurrent chats cannot overwrite a newer snapshot.
func (c *Client) persistHistoryLocked() error {
	if c.historyErr != nil {
		return c.historyErr
	}
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
	cipher, err := storagecrypt.New(c.cfg.StorageEncryptionKey)
	if err != nil {
		return err
	}
	if cipher != nil {
		data = append([]byte(encryptedHistoryPrefix), cipher.Seal(data, []byte(encryptedHistoryPrefix+filepath.Base(path)))...)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
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
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	return parent.Sync()
}
