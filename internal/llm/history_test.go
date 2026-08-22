package llm

import (
	"path/filepath"
	"testing"

	"github.com/hyphentae/nyande-bot/internal/config"
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
