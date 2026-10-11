package llm

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
)

func TestHistorySurvivesClientRestartAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	cfg := config.Config{LLMHistoryFile: path, LLMMaxHistory: 4}
	client := New(cfg)
	client.saveHistory(42, "first user", "first assistant")
	client.saveHistory(42, "second user", "second assistant")

	reloaded := New(cfg)
	if len(reloaded.history[42]) != 4 {
		t.Fatalf("loaded %d messages, want 4", len(reloaded.history[42]))
	}
	if reloaded.history[42][0].Content != "first user" || reloaded.history[42][3].Content != "second assistant" {
		t.Fatalf("unexpected loaded history: %#v", reloaded.history[42])
	}
	if !reloaded.Reset(42) {
		t.Fatal("reset did not find persisted history")
	}
	if afterReset := New(cfg); len(afterReset.history[42]) != 0 {
		t.Fatalf("history survived reset: %#v", afterReset.history[42])
	}
}

func TestHistoryLoadAppliesCurrentLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	writer := New(config.Config{LLMHistoryFile: path, LLMMaxHistory: 10})
	writer.saveHistory(7, "one", "two")
	writer.saveHistory(7, "three", "four")

	reader := New(config.Config{LLMHistoryFile: path, LLMMaxHistory: 2})
	if len(reader.history[7]) != 2 || reader.history[7][0].Content != "three" {
		t.Fatalf("history limit was not applied: %#v", reader.history[7])
	}
}

func TestEncryptedHistoryMigratesAndSurvivesRestartAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "history.json")
	cfg := config.Config{LLMHistoryFile: path, LLMMaxHistory: 4}
	New(cfg).saveHistory(42, "private user message", "private assistant reply")
	cfg.StorageEncryptionKey = bytes.Repeat([]byte{17}, 32)
	migrated := New(cfg)
	if migrated.historyErr != nil || len(migrated.history[42]) != 2 {
		t.Fatalf("migration failed: history=%#v err=%v", migrated.history, migrated.historyErr)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte(encryptedHistoryPrefix)) || bytes.Contains(data, []byte("private")) {
		t.Fatalf("history was not encrypted: err=%v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("history file must have mode 0600: %v", err)
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("new history directory must have mode 0700: %v", err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("migration left temporary files: %v (%v)", files, err)
	}
	reloaded := New(cfg)
	if reloaded.historyErr != nil || len(reloaded.history[42]) != 2 || reloaded.history[42][0].Content != "private user message" || reloaded.history[42][1].Content != "private assistant reply" {
		t.Fatalf("encrypted history did not survive restart: history=%#v err=%v", reloaded.history, reloaded.historyErr)
	}
	reloaded.saveHistory(42, "another private message", "another private reply")
	if afterSave := New(cfg); afterSave.historyErr != nil || len(afterSave.history[42]) != 4 || afterSave.history[42][3].Content != "another private reply" {
		t.Fatalf("encrypted save failed: history=%#v err=%v", afterSave.history, afterSave.historyErr)
	}
	if !reloaded.Reset(42) {
		t.Fatal("reset did not find encrypted history")
	}
	if afterReset := New(cfg); afterReset.historyErr != nil || len(afterReset.history[42]) != 0 {
		t.Fatalf("encrypted reset failed: history=%#v err=%v", afterReset.history, afterReset.historyErr)
	}
	data, err = os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte(encryptedHistoryPrefix)) || bytes.Contains(data, []byte("private")) {
		t.Fatalf("reset removed encryption: %v", err)
	}
}

func TestHistoryLoadFailureCannotOverwriteStoredData(t *testing.T) {
	key := bytes.Repeat([]byte{17}, 32)
	source := filepath.Join(t.TempDir(), "history.json")
	New(config.Config{LLMHistoryFile: source, StorageEncryptionKey: key}).saveHistory(42, "original user text", "original answer")
	encrypted, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(encrypted)
	tampered[len(tampered)-1] ^= 1
	for _, test := range []struct {
		name, filename string
		key, data      []byte
	}{
		{name: "wrong key", key: bytes.Repeat([]byte{99}, 32), data: encrypted},
		{name: "missing key", data: encrypted},
		{name: "invalid key", key: []byte("short"), data: encrypted},
		{name: "tampered ciphertext", key: key, data: tampered},
		{name: "truncated ciphertext", key: key, data: []byte(encryptedHistoryPrefix + "short")},
		{name: "swapped history", filename: "history.json.discord", key: key, data: encrypted},
		{name: "corrupt legacy JSON", key: key, data: []byte(`{"version":1,"chats":`)},
		{name: "unsupported legacy version", key: key, data: []byte(`{"version":99,"chats":{}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename := test.filename
			if filename == "" {
				filename = "history.json"
			}
			path := filepath.Join(t.TempDir(), filename)
			if err := os.WriteFile(path, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			client := New(config.Config{LLMEnabled: true, LLMHistoryFile: path, StorageEncryptionKey: test.key})
			if client.historyErr == nil {
				t.Fatal("invalid history did not block the client")
			}
			client.http.Transport = tikTokTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("unavailable history must block HTTP requests")
				return nil, nil
			})
			if _, err := client.Ask(context.Background(), Request{ChatID: 42, Text: "new question"}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
				t.Fatalf("Ask did not report protected history: %v", err)
			}
			// Exercise the guards even if a failed migration had populated memory.
			client.history[42] = []chatMessage{{Role: "user", Content: "kept in memory"}}
			if client.Reset(42) {
				t.Fatal("reset accepted unavailable history")
			}
			client.saveHistory(42, "replacement", "replacement")
			if err := client.persistHistoryLocked(); err == nil {
				t.Fatal("persist accepted unavailable history")
			}
			if len(client.history[42]) != 1 || client.history[42][0].Content != "kept in memory" {
				t.Fatal("failed history operation changed in-memory data")
			}
			stored, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(stored, test.data) {
				t.Fatalf("unreadable history was overwritten: %v", err)
			}
		})
	}
}
