package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

type agentTestRequest struct {
	Method    string
	ChatID    int64  `json:"chat_id"`
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
}

type agentTestServer struct {
	mu       sync.Mutex
	requests []agentTestRequest
	llm      func(*http.Request) (string, error)
	telegram func(*http.Request, agentTestRequest) error
}

func newAgentTestBot(t *testing.T, respond func(*http.Request) (string, error)) (*Bot, *agentTestServer, context.Context) {
	t.Helper()
	b := concurrentTestBot()
	server := &agentTestServer{llm: respond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true,"result":{"message_id":100}}`
		if r.URL.Hostname() == "llm.test" {
			var err error
			body, err = server.llm(r)
			if err != nil {
				return nil, err
			}
		} else if r.URL.Hostname() == "api.telegram.org" {
			var request agentTestRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			request.Method = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			server.mu.Lock()
			server.requests = append(server.requests, request)
			server.mu.Unlock()
			if server.telegram != nil {
				if err := server.telegram(r, request); err != nil {
					return nil, err
				}
			}
			if request.Method == "sendChatAction" {
				body = `{"ok":true,"result":true}`
			}
		} else {
			t.Errorf("unexpected network destination: %s", r.URL.Hostname())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() {
		cancel()
		b.agentShutdown()
		http.DefaultTransport = previous
	})
	return b, server, ctx
}

func (s *agentTestServer) snapshot() []agentTestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentTestRequest(nil), s.requests...)
}

func agentTestMessage(chatID, userID int64, text string) *telegram.Message {
	return &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: chatID, Type: "private"}, From: &telegram.User{ID: userID}, Text: text}
}

func waitAgentTest(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent operation did not finish")
	}
}

func waitAgentJobs(t *testing.T, b *Bot) {
	t.Helper()
	done := make(chan struct{})
	go func() { b.agentWG.Wait(); close(done) }()
	waitAgentTest(t, done)
}

func TestAgentControlsOwnershipIsolationAndResponsiveness(t *testing.T) {
	started := make(chan context.Context, 4)
	b, server, ctx := newAgentTestBot(t, func(r *http.Request) (string, error) {
		started <- r.Context()
		<-r.Context().Done()
		return "", r.Context().Err()
	})
	message := agentTestMessage(1, 10, "/agent private task argument")
	if err := b.HandleUpdate(ctx, telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	var taskCtx context.Context
	select {
	case taskCtx = <-started:
	case <-ctx.Done():
		t.Fatal("background task did not start")
	}
	// The task must not retain the ordinary update lock or block /ping.
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		if err := b.HandleUpdate(ctx, telegram.Update{Message: agentTestMessage(1, 20, "/ping")}); err != nil {
			t.Error(err)
		}
	}()
	waitAgentTest(t, pingDone)
	// Controls also bypass a lock held by an unrelated conversation update.
	unlock := b.lockUpdates(1, false)
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		if err := b.HandleUpdate(ctx, telegram.Update{Message: agentTestMessage(1, 20, "/agentstatus")}); err != nil {
			t.Error(err)
		}
	}()
	waitAgentTest(t, statusDone)
	unlock()
	for _, request := range []*telegram.Message{agentTestMessage(1, 20, "/agentstop"), agentTestMessage(2, 10, "/agentstop")} {
		if err := b.HandleUpdate(ctx, telegram.Update{Message: request}); err != nil {
			t.Fatal(err)
		}
	}
	if taskCtx.Err() != nil {
		t.Fatal("another user or chat cancelled the task")
	}
	if err := b.HandleUpdate(ctx, telegram.Update{Message: agentTestMessage(1, 10, "/agentstop")}); err != nil {
		t.Fatal(err)
	}
	waitAgentJobs(t, b)
	ownerDenied, otherChatEmpty, stopped := false, false, false
	for _, request := range server.snapshot() {
		ownerDenied = ownerDenied || strings.Contains(request.Text, "только тот")
		otherChatEmpty = otherChatEmpty || (request.ChatID == 2 && request.Text == agentNoTask)
		stopped = stopped || (request.Method == "editMessageText" && request.Text == agentStopped)
		if strings.Contains(request.Text, "private task argument") {
			t.Fatal("status leaked task contents")
		}
	}
	if !ownerDenied || !otherChatEmpty || !stopped {
		t.Fatalf("owner rejection=%v other chat empty=%v stopped=%v", ownerDenied, otherChatEmpty, stopped)
	}
}

func TestAgentAdmissionLimitsAndShutdown(t *testing.T) {
	started := make(chan struct{}, 4)
	b, server, ctx := newAgentTestBot(t, func(r *http.Request) (string, error) {
		started <- struct{}{}
		<-r.Context().Done()
		return "", r.Context().Err()
	})
	for _, chatID := range []int64{1, 2, 3, 1, 4} {
		if err := b.agentCommand(ctx, agentTestMessage(chatID, 10, ""), "task"); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		waitAgentTest(t, started)
	}
	b.agentMu.Lock()
	count := len(b.agentJobs)
	b.agentMu.Unlock()
	if count != agentMaximumJobs {
		t.Fatalf("running jobs=%d, want 3", count)
	}
	busyChat, busyGlobal := false, false
	for _, request := range server.snapshot() {
		busyChat = busyChat || strings.Contains(request.Text, "уже выполняется")
		busyGlobal = busyGlobal || strings.Contains(request.Text, "три задачи")
	}
	if !busyChat || !busyGlobal {
		t.Fatalf("per-chat busy=%v global busy=%v", busyChat, busyGlobal)
	}
	b.agentShutdown()
	before := len(server.snapshot())
	if err := b.agentCommand(ctx, agentTestMessage(5, 10, ""), "task"); err != nil {
		t.Fatal(err)
	}
	if len(server.snapshot()) != before || len(b.agentJobs) != 0 {
		t.Fatal("shutdown admitted work or retained jobs")
	}
	for _, request := range server.snapshot() {
		if request.Method == "editMessageText" {
			t.Fatal("shutdown sent a terminal notification")
		}
	}
}

func TestAgentShutdownCancelsInitialSend(t *testing.T) {
	b, server, ctx := newAgentTestBot(t, func(r *http.Request) (string, error) {
		t.Error("LLM started before the initial status was sent")
		return "", errors.New("unexpected LLM call")
	})
	started := make(chan struct{})
	server.telegram = func(r *http.Request, request agentTestRequest) error {
		if request.Text == agentStarting {
			close(started)
			<-r.Context().Done()
			return r.Context().Err()
		}
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.agentCommand(ctx, agentTestMessage(1, 10, ""), "task")
	}()
	waitAgentTest(t, started)
	b.agentShutdown()
	waitAgentTest(t, done)
	if len(b.agentJobs) != 0 {
		t.Fatal("failed initial send retained job")
	}
}

func TestAgentCompletionFailureAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name, response, status string
		failure                error
	}{
		{name: "success", response: `{"choices":[{"message":{"content":"**result**"}}]}`, status: agentCompleted},
		{name: "error", failure: errors.New("provider-secret-token"), status: agentFailed},
		{name: "provider timeout", failure: context.DeadlineExceeded, status: agentFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, server, ctx := newAgentTestBot(t, func(r *http.Request) (string, error) { return test.response, test.failure })
			if err := b.agentCommand(ctx, agentTestMessage(1, 10, ""), "task"); err != nil {
				t.Fatal(err)
			}
			waitAgentJobs(t, b)
			found, answer := false, false
			for _, request := range server.snapshot() {
				found = found || (request.Method == "editMessageText" && request.Text == test.status && request.MessageID == 100)
				answer = answer || request.Text == "result"
				if strings.Contains(request.Text, "provider-secret-token") {
					t.Fatal("status exposed provider error")
				}
			}
			if !found || (test.failure == nil && !answer) {
				t.Fatalf("terminal status=%v answer=%v", found, answer)
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		b, server, parent := newAgentTestBot(t, nil)
		ctx, cancel := context.WithDeadline(parent, time.Now().Add(-time.Second))
		job := &agentJob{message: *agentTestMessage(1, 10, "task"), cancel: cancel, statusID: 100}
		b.agentJobs = map[int64]*agentJob{1: job}
		b.agentWG.Add(1)
		b.runAgent(parent, ctx, job)
		requests := server.snapshot()
		if len(requests) != 1 || requests[0].Text != agentTimedOut {
			t.Fatalf("deadline response=%+v", requests)
		}
	})
}

func TestAgentProgressThrottleAndPermissionRevocation(t *testing.T) {
	b, server, parent := newAgentTestBot(t, nil)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	job := &agentJob{message: *agentTestMessage(1, 10, "private task"), cancel: cancel, statusID: 100}
	progress := llm.AgentProgress{Step: 2, MaxSteps: 12, ToolCalls: 1, Phase: "tool", Tool: "provider-secret-name"}
	b.agentProgress(ctx, job, progress)
	b.agentProgress(ctx, job, progress)
	requests := server.snapshot()
	if len(requests) != 1 || strings.Contains(requests[0].Text, "provider-secret-name") || strings.Contains(requests[0].Text, "private task") {
		t.Fatalf("unthrottled or unsafe progress: %+v", requests)
	}
	b.llmConfig = &llmChatSettings{disabledChats: map[int64]struct{}{1: {}}}
	b.agentProgress(ctx, job, progress)
	if !errors.Is(ctx.Err(), context.Canceled) || len(server.snapshot()) != 1 {
		t.Fatal("permission revocation must cancel without output")
	}
}

func TestAgentRejectsMissingIdentityBotsAndEmptyTasks(t *testing.T) {
	b, server, ctx := newAgentTestBot(t, nil)
	missing, bot := agentTestMessage(1, 10, ""), agentTestMessage(1, 10, "")
	missing.From = nil
	bot.From.IsBot = true
	for _, message := range []*telegram.Message{missing, bot} {
		if err := b.agentCommand(ctx, message, "task"); err != nil {
			t.Fatal(err)
		}
	}
	if len(server.snapshot()) != 0 || len(b.agentJobs) != 0 {
		t.Fatal("unidentified sender or bot started a task")
	}
	if err := b.agentCommand(ctx, agentTestMessage(1, 10, ""), " \n "); err != nil {
		t.Fatal(err)
	}
	requests := server.snapshot()
	if len(requests) != 1 || !strings.Contains(requests[0].Text, "/agent <") || len(b.agentJobs) != 0 {
		t.Fatalf("empty task response=%+v", requests)
	}
}

func TestAgentPermissionCancellationIsScoped(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(*Bot)
		want   map[int64]bool
	}{
		{name: "chat", cancel: func(b *Bot) { b.cancelAgentChat(1) }, want: map[int64]bool{1: true}},
		{name: "user across chats", cancel: func(b *Bot) { b.cancelAgentUser(10) }, want: map[int64]bool{1: true, 2: true}},
		{name: "absent chat", cancel: func(b *Bot) { b.cancelAgentChat(9) }, want: map[int64]bool{}},
		{name: "absent user", cancel: func(b *Bot) { b.cancelAgentUser(99) }, want: map[int64]bool{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := &Bot{agentJobs: make(map[int64]*agentJob)}
			contexts := make(map[int64]context.Context)
			for chatID, ownerID := range map[int64]int64{1: 10, 2: 10, 3: 20} {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				contexts[chatID] = ctx
				b.agentJobs[chatID] = &agentJob{ownerID: ownerID, cancel: cancel}
			}
			test.cancel(b)
			for chatID, ctx := range contexts {
				if cancelled := errors.Is(ctx.Err(), context.Canceled); cancelled != test.want[chatID] {
					t.Errorf("chat %d cancelled=%v, want %v", chatID, cancelled, test.want[chatID])
				}
			}
		})
	}
}

func TestAgentPermissionCancellationStopsRemainingAnswerChunks(t *testing.T) {
	answer := strings.Repeat("chunk ", 1800)
	response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": answer}}}})
	if err != nil {
		t.Fatal(err)
	}
	b, server, ctx := newAgentTestBot(t, func(r *http.Request) (string, error) { return string(response), nil })
	b.llmConfig = &llmChatSettings{disabledChats: make(map[int64]struct{})}
	server.telegram = func(r *http.Request, request agentTestRequest) error {
		if request.Method == "sendMessage" && strings.HasPrefix(request.Text, "chunk ") {
			// The first chunk has already reached Telegram when permissions
			// change. Later chunks must observe cancellation before any HTTP.
			b.llmConfig.mu.Lock()
			b.llmConfig.disabledChats[1] = struct{}{}
			b.llmConfig.mu.Unlock()
			b.cancelAgentChat(1)
		}
		return nil
	}
	if err := b.agentCommand(ctx, agentTestMessage(1, 10, ""), "make a long answer"); err != nil {
		t.Fatal(err)
	}
	waitAgentJobs(t, b)
	chunks := 0
	for _, request := range server.snapshot() {
		if request.Method == "sendMessage" && strings.HasPrefix(request.Text, "chunk ") {
			chunks++
		}
		if request.Method == "editMessageText" {
			t.Fatal("task sent a terminal status after permissions were revoked")
		}
	}
	if chunks != 1 {
		t.Fatalf("delivered chunks=%d, want the single in-flight chunk", chunks)
	}
}
