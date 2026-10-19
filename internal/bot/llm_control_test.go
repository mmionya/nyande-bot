package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/chatlog"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/schedules"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

type llmControlNetwork struct {
	mu       sync.Mutex
	requests []string
	messages []string
	typed    chan struct{}
	videos   int
}

func llmControlTestBot(t *testing.T) (*Bot, *llmControlNetwork) {
	t.Helper()
	b := concurrentTestBot()
	b.identity = telegram.User{ID: 99, Username: "nyande_test_bot"}
	settings, err := loadLLMChatSettings(filepath.Join(t.TempDir(), "llm.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.llmConfig = settings
	archive, err := chatlog.Open(filepath.Join(t.TempDir(), "archive.db"), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.chatlog = archive
	t.Cleanup(func() { _ = archive.Close() })
	b.adminCache = map[adminKey]adminEntry{
		{ChatID: -1, UserID: 1}: {admin: true, expires: time.Now().Add(time.Hour)},
		{ChatID: -1, UserID: 2}: {admin: false, expires: time.Now().Add(time.Hour)},
	}
	network := &llmControlNetwork{typed: make(chan struct{}, 10)}
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body, contentType := `{"ok":true,"result":{"message_id":100}}`, "application/json"
		switch r.URL.Hostname() {
		case "llm.test":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			// Wait for the typing goroutine's first request so it cannot outlive
			// the mock transport when the test finishes.
			select {
			case <-network.typed:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			network.mu.Lock()
			network.requests = append(network.requests, string(data))
			network.mu.Unlock()
			body = `{"choices":[{"message":{"content":"Mocked LLM answer"}}]}`
		case "8.8.8.8":
			body, contentType = "mock video bytes", "video/mp4"
		case "api.telegram.org":
			switch {
			case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
				network.typed <- struct{}{}
				body = `{"ok":true,"result":true}`
			case strings.HasSuffix(r.URL.Path, "/sendMessage"):
				var payload struct{ Text string }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				network.mu.Lock()
				network.messages = append(network.messages, payload.Text)
				network.mu.Unlock()
			case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
				body = `{"ok":true,"result":true}`
			case strings.HasSuffix(r.URL.Path, "/sendVideo"):
				data, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(data), "mock video bytes") {
					t.Errorf("downloaded video missing from upload: %v", err)
				}
				network.mu.Lock()
				network.videos++
				network.mu.Unlock()
			case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
				body = `{"ok":true,"result":true}`
			case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			default:
				t.Errorf("unexpected Telegram request: %s", r.URL.Path)
			}
		default:
			t.Errorf("unexpected network request: %s", r.URL.Hostname())
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return b, network
}

func llmControlMessage(id int, chatID, author int64, text string) *telegram.Message {
	chatType := "private"
	if chatID < 0 {
		chatType = "supergroup"
	}
	return &telegram.Message{MessageID: id, Chat: telegram.Chat{ID: chatID, Type: chatType}, From: &telegram.User{ID: author}, Text: text}
}

func llmControlUpdate(t *testing.T, b *Bot, message *telegram.Message, edited bool) {
	t.Helper()
	update := telegram.Update{Message: message}
	if edited {
		update = telegram.Update{EditedMessage: message}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
}

func TestLLMOffStopsCollectionAndRequestsOnlyInSelectedChat(t *testing.T) {
	b, network := llmControlTestBot(t)
	ctx := context.Background()
	b.saveChatMessage(ctx, llmControlMessage(1, 1, 1, "Previously archived message"))
	llmControlUpdate(t, b, llmControlMessage(2, 1, 1, "/llm off"), false)
	if !b.llmConfig.Disabled(1) || b.llmConfig.Disabled(2) {
		t.Fatal("/llm off did not stay scoped to the selected chat")
	}
	archive, err := b.chatlog.Recent(ctx, 1, 100)
	if err != nil || len(archive) != 1 || archive[0].Text != "Previously archived message" {
		t.Fatalf("off command entered the archive or changed earlier messages: %#v, %v", archive, err)
	}
	for _, message := range []*telegram.Message{
		llmControlMessage(3, 1, 1, "This text must stay private while off"),
		llmControlMessage(4, 1, 1, "https://example.com/never-send-this-to-the-llm"),
	} {
		llmControlUpdate(t, b, message, false)
	}
	llmControlUpdate(t, b, llmControlMessage(1, 1, 1, "Edited while off"), true)
	caption := llmControlMessage(5, 1, 1, "")
	caption.Photo = []telegram.PhotoSize{{FileID: "must-not-download"}}
	caption.Caption = "Photo caption while off"
	llmControlUpdate(t, b, caption, false)
	llmControlUpdate(t, b, caption, true)
	archive, err = b.chatlog.Recent(ctx, 1, 100)
	if err != nil || len(archive) != 1 || archive[0].Text != "Previously archived message" {
		t.Fatalf("disabled chat continued collecting messages or edits: %#v, %v", archive, err)
	}
	network.mu.Lock()
	calls := len(network.requests)
	beforePing := len(network.messages)
	network.mu.Unlock()
	if calls != 0 {
		t.Fatalf("disabled chat made %d LLM requests", calls)
	}
	llmControlUpdate(t, b, llmControlMessage(6, 1, 1, "/ping"), false)
	network.mu.Lock()
	if len(network.messages) != beforePing+1 {
		t.Error("turning off the LLM disabled an ordinary bot command")
	}
	network.mu.Unlock()
	llmControlUpdate(t, b, llmControlMessage(1, 2, 2, "Other chat still works"), false)
	network.mu.Lock()
	calls = len(network.requests)
	network.mu.Unlock()
	if calls != 1 {
		t.Fatalf("another chat lost LLM access: requests=%d", calls)
	}
	reloaded, err := loadLLMChatSettings(b.llmConfig.path)
	if err != nil {
		t.Fatal(err)
	}
	b.llmConfig = reloaded
	if !b.llmConfig.Disabled(1) || b.llmConfig.Disabled(2) {
		t.Fatal("per-chat off setting was not persisted")
	}
	llmControlUpdate(t, b, llmControlMessage(7, 1, 1, "/llm on"), false)
	llmControlUpdate(t, b, llmControlMessage(8, 1, 1, "A message after enabling"), false)
	archive, err = b.chatlog.Recent(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	resumed := false
	for _, message := range archive {
		resumed = resumed || message.Text == "A message after enabling"
		if strings.Contains(message.Text, "while off") || strings.Contains(message.Text, "never-send") {
			t.Errorf("message from disabled period was collected later: %s", message.Text)
		}
	}
	if !resumed {
		t.Fatal("message collection did not resume")
	}
	network.mu.Lock()
	defer network.mu.Unlock()
	if len(network.requests) != 2 || !strings.Contains(network.requests[1], "A message after enabling") {
		t.Fatalf("LLM did not resume: %#v", network.requests)
	}
	for _, request := range network.requests {
		if strings.Contains(request, "while off") || strings.Contains(request, "never-send") {
			t.Error("disabled-period text leaked into a later LLM request")
		}
	}
}

func TestLLMControlAdminAuthorizationAndCommandRecipient(t *testing.T) {
	b, _ := llmControlTestBot(t)
	llmControlUpdate(t, b, llmControlMessage(1, -1, 2, "/llm off"), false)
	if b.llmConfig.Disabled(-1) {
		t.Fatal("non-administrator disabled the LLM")
	}
	llmControlUpdate(t, b, llmControlMessage(2, -1, 1, "/llm@another_bot off"), false)
	if b.llmConfig.Disabled(-1) {
		t.Fatal("command addressed to another bot disabled the LLM")
	}
	llmControlUpdate(t, b, llmControlMessage(3, -1, 1, "/llm@nyande_test_bot off"), false)
	if !b.llmConfig.Disabled(-1) {
		t.Fatal("administrator could not disable the LLM")
	}
	llmControlUpdate(t, b, llmControlMessage(4, -1, 2, "/llm on"), false)
	llmControlUpdate(t, b, llmControlMessage(5, -1, 1, "/llm@another_bot on"), false)
	if !b.llmConfig.Disabled(-1) {
		t.Fatal("non-administrator or another bot's command enabled the LLM")
	}
	llmControlUpdate(t, b, llmControlMessage(6, -1, 1, "/llm on"), false)
	if b.llmConfig.Disabled(-1) {
		t.Fatal("administrator could not enable the LLM")
	}
}

func TestLLMOffKeepsDirectVideoDownloadsEnabled(t *testing.T) {
	b, network := llmControlTestBot(t)
	llmControlUpdate(t, b, llmControlMessage(1, 1, 1, "/llm off"), false)
	llmControlUpdate(t, b, llmControlMessage(2, 1, 1, "https://8.8.8.8/video.mp4"), false)
	network.mu.Lock()
	defer network.mu.Unlock()
	if network.videos != 1 || len(network.requests) != 0 {
		t.Fatalf("off state affected media delivery: videos=%d LLM requests=%d", network.videos, len(network.requests))
	}
	archive, err := b.chatlog.Recent(context.Background(), 1, 100)
	if err != nil || len(archive) != 0 {
		t.Fatalf("media URL was archived while the LLM was off: %#v, %v", archive, err)
	}
}

func TestLLMControlStatusAndInvalidArgumentsDoNotChangeSetting(t *testing.T) {
	b, network := llmControlTestBot(t)
	for _, disabled := range []bool{false, true} {
		if err := b.llmConfig.SetDisabled(-1, disabled); err != nil {
			t.Fatal(err)
		}
		for i, command := range []string{"/llm", "/llm maybe", "/llm on off"} {
			network.mu.Lock()
			before := len(network.messages)
			network.mu.Unlock()
			llmControlUpdate(t, b, llmControlMessage(10+i, -1, 1, command), false)
			if b.llmConfig.Disabled(-1) != disabled {
				t.Fatalf("%q changed state from disabled=%v", command, disabled)
			}
			network.mu.Lock()
			if len(network.messages) != before+1 {
				t.Errorf("%q did not reply with status or usage", command)
			}
			network.mu.Unlock()
		}
	}
}

func TestLLMControlOnResumesCollectionWhenLLMGloballyDisabled(t *testing.T) {
	b, network := llmControlTestBot(t)
	b.cfg.LLMEnabled = false
	b.llm = llm.New(b.cfg)
	llmControlUpdate(t, b, llmControlMessage(1, 1, 1, "/llm off"), false)
	llmControlUpdate(t, b, llmControlMessage(2, 1, 1, "Do not collect this message"), false)
	archive, err := b.chatlog.Recent(context.Background(), 1, 100)
	if err != nil || len(archive) != 0 {
		t.Fatalf("off did not stop collection with the LLM globally disabled: %#v, %v", archive, err)
	}
	llmControlUpdate(t, b, llmControlMessage(3, 1, 1, "/llm on"), false)
	if b.llmConfig.Disabled(1) {
		t.Fatal("global LLM disable prevented clearing the per-chat collection override")
	}
	reloaded, err := loadLLMChatSettings(b.llmConfig.path)
	if err != nil || reloaded.Disabled(1) {
		t.Fatalf("cleared collection override was not persisted: %v", err)
	}
	llmControlUpdate(t, b, llmControlMessage(4, 1, 1, "Collection is enabled again"), false)
	archive, err = b.chatlog.Recent(context.Background(), 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	collected := false
	for _, message := range archive {
		collected = collected || message.Text == "Collection is enabled again"
		if message.Text == "Do not collect this message" {
			t.Error("disabled-period message was archived after enabling")
		}
	}
	if !collected {
		t.Fatal("/llm on did not resume collection when generation is globally disabled")
	}
	llmControlUpdate(t, b, llmControlMessage(5, 1, 1, "/llm"), false)
	network.mu.Lock()
	defer network.mu.Unlock()
	if len(network.requests) != 0 {
		t.Fatal("per-chat on overrode the global LLM disable")
	}
	if last := network.messages[len(network.messages)-1]; last != resources.Get("llm.control.global_disabled_collecting") {
		t.Fatalf("status did not explain that collection works but the LLM is globally disabled: %q", last)
	}
}

func TestLLMOffBlocksExplicitScheduleRecognitionWithoutLosingDraft(t *testing.T) {
	b, network := llmControlTestBot(t)
	ctx := context.Background()
	store, err := schedules.Open(filepath.Join(t.TempDir(), "schedules.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b.schedules = store
	draft, err := store.SaveDraft(ctx, schedules.Draft{UserID: 1, ChatID: 1, Stage: "collect", Notes: []string{"Завтра в 15:00 математика"}})
	if err != nil {
		t.Fatal(err)
	}
	llmControlUpdate(t, b, llmControlMessage(2, 1, 1, "/llm off"), false)
	callback := &telegram.CallbackQuery{ID: "recognize-while-off", From: telegram.User{ID: 1}, Message: llmControlMessage(100, 1, 99, ""), Data: fmt.Sprintf("sch:recognize:%d", draft.Revision)}
	if err := b.HandleUpdate(ctx, telegram.Update{CallbackQuery: callback}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.GetDraft(ctx, 1, 1)
	if err != nil || unchanged.Revision != draft.Revision || len(unchanged.Notes) != 1 {
		t.Fatalf("refused recognition lost or modified the draft: %#v, %v", unchanged, err)
	}
	network.mu.Lock()
	defer network.mu.Unlock()
	if len(network.requests) != 0 {
		t.Fatal("explicit recognition bypassed the per-chat LLM setting")
	}
}
