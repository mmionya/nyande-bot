package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

const (
	agentTimeout          = 10 * time.Minute
	agentProgressInterval = 2 * time.Second
	agentMaximumJobs      = 3
	agentMaximumSteps     = 12
	agentMaximumToolCalls = 36
	agentStarting         = "Запускаю задачу…\n/agentstatus — состояние\n/agentstop — остановить"
	agentNoTask           = "В этом чате нет выполняющейся задачи. Запуск: /agent <что нужно сделать>."
	agentStopped          = "Задача остановлена."
	agentCompleted        = "Задача завершена. Результат отправлен выше."
	agentFailed           = "Не удалось завершить задачу. Попробуй уточнить запрос или запустить её позже."
	agentTimedOut         = "Задача остановлена: достигнут лимит времени (10 минут). Можно запустить более короткую задачу."
)

// All mutable job metadata is protected by Bot.agentMu. outputMu serializes
// edits so a late progress response cannot overwrite the terminal status.
type agentJob struct {
	message  telegram.Message
	ownerID  int64
	cancel   context.CancelFunc
	statusID int
	progress llm.AgentProgress
	lastEdit time.Time
	stopping bool
	outputMu sync.Mutex
}

func (b *Bot) agentOutputAllowed(message *telegram.Message) bool {
	if message == nil || b.llmDisabled(message.Chat.ID) || b.silentModeration(message.Chat.ID) || b.blockedMessage(message) {
		return false
	}
	b.agentMu.Lock()
	closing := b.agentClosing
	b.agentMu.Unlock()
	return !closing
}

func (b *Bot) agentCommand(ctx context.Context, message *telegram.Message, arguments string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if message == nil || message.From == nil || message.From.IsBot || message.SenderChat != nil || b.blockedMessage(message) || b.silentModeration(message.Chat.ID) {
		return nil
	}
	if b.llm == nil || !b.llm.Enabled() || b.llmDisabled(message.Chat.ID) {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("bot.agent.disabled"), message.MessageID, nil)
		return err
	}
	respond := func(text string) error { return b.agentReply(ctx, message, text) }
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return respond("Напиши задачу после команды: /agent <что нужно сделать>. Например: /agent сравни эти источники и подготовь выводы. Можно ответить командой на сообщение с файлом.")
	}
	request := *message
	user := *message.From
	request.From = &user
	request.Text, request.Caption = arguments, ""
	request.Entities, request.CaptionEntities = nil, nil
	taskCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	job := &agentJob{message: request, ownerID: user.ID, cancel: cancel, progress: llm.AgentProgress{MaxSteps: agentMaximumSteps}}
	b.agentMu.Lock()
	if b.agentClosing {
		b.agentMu.Unlock()
		cancel()
		return nil
	}
	if b.agentJobs[message.Chat.ID] != nil {
		b.agentMu.Unlock()
		cancel()
		return respond("В этом чате уже выполняется задача. /agentstatus — состояние, /agentstop — остановить свою задачу.")
	}
	if len(b.agentJobs) >= agentMaximumJobs {
		b.agentMu.Unlock()
		cancel()
		return respond("Сейчас выполняются три задачи. Попробуй запустить свою чуть позже.")
	}
	if b.agentJobs == nil {
		b.agentJobs = make(map[int64]*agentJob)
	}
	b.agentJobs[message.Chat.ID] = job
	// Add and the closing check share the same lock as Shutdown, including
	// the initial send, so Shutdown cannot race Wait against a new Add.
	b.agentWG.Add(1)
	b.agentMu.Unlock()
	if !b.agentOutputAllowed(&job.message) {
		b.agentFinish(job)
		return nil
	}
	status, err := b.telegram.SendMessage(taskCtx, message.Chat.ID, agentStarting, message.MessageID, nil)
	if err != nil {
		b.agentFinish(job)
		return err
	}
	b.agentMu.Lock()
	job.statusID, job.lastEdit = status.MessageID, time.Now()
	b.agentMu.Unlock()
	go b.runAgent(ctx, taskCtx, job)
	return nil
}

func (b *Bot) runAgent(parent, ctx context.Context, job *agentJob) {
	defer b.agentFinish(job)
	options := &llm.AgentOptions{
		MaxSteps: agentMaximumSteps, MaxToolCalls: agentMaximumToolCalls,
		OnProgress: func(progress llm.AgentProgress) { b.agentProgress(ctx, job, progress) },
	}
	err := ctx.Err()
	if err == nil && !b.agentOutputAllowed(&job.message) {
		job.cancel()
		err = context.Canceled
	}
	if err == nil {
		err = b.handleLLMRequest(ctx, &job.message, options)
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if parent.Err() != nil || !b.agentOutputAllowed(&job.message) {
		return
	}
	status := agentCompleted
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status = agentTimedOut
	case errors.Is(err, context.Canceled):
		status = agentStopped
	case err != nil:
		status = agentFailed
	}
	// User cancellation ends work, but its terminal edit uses the still-live
	// parent. Process shutdown cancels the parent or closes the output gate.
	finishCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	job.outputMu.Lock()
	defer job.outputMu.Unlock()
	if finishCtx.Err() == nil && b.agentOutputAllowed(&job.message) {
		_ = b.telegram.EditMessageText(finishCtx, job.message.Chat.ID, job.statusID, status, nil)
	}
}

func (b *Bot) agentProgress(ctx context.Context, job *agentJob, progress llm.AgentProgress) {
	if ctx.Err() != nil || !b.agentOutputAllowed(&job.message) {
		job.cancel()
		return
	}
	job.outputMu.Lock()
	defer job.outputMu.Unlock()
	b.agentMu.Lock()
	job.progress = progress
	edit := !job.stopping && job.statusID != 0 && time.Since(job.lastEdit) >= agentProgressInterval
	if edit {
		job.lastEdit = time.Now()
	}
	statusID := job.statusID
	b.agentMu.Unlock()
	if !edit || ctx.Err() != nil || !b.agentOutputAllowed(&job.message) {
		return
	}
	editCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = b.telegram.EditMessageText(editCtx, job.message.Chat.ID, statusID, agentProgressText(progress), nil)
}

func agentProgressText(progress llm.AgentProgress) string {
	phase := "Обрабатываю задачу."
	if progress.Phase == "finalizing" {
		phase = "Подготавливаю результат."
	} else if progress.Phase == "tool" {
		switch progress.Tool {
		case "web_search", "search_web", "search":
			phase = "Ищу источники."
		case "read_webpage":
			phase = "Читаю источник."
		case "create_file":
			phase = "Создаю файл."
		case "search_chat_messages", "get_recent_messages":
			phase = "Изучаю сообщения этого чата."
		default:
			phase = "Использую инструмент."
		}
	}
	return fmt.Sprintf("%s\nШаг %d/%d · вызовов инструментов: %d\n/agentstatus — состояние · /agentstop — остановить", phase,
		max(0, min(progress.Step, agentMaximumSteps)), agentMaximumSteps, max(0, min(progress.ToolCalls, agentMaximumToolCalls)))
}

func (b *Bot) agentControlCommand(ctx context.Context, message *telegram.Message, command string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.agentOutputAllowed(message) {
		return nil
	}
	b.agentMu.Lock()
	job := b.agentJobs[message.Chat.ID]
	if job == nil {
		b.agentMu.Unlock()
		return b.agentReply(ctx, message, agentNoTask)
	}
	progress, stopping := job.progress, job.stopping
	owner := message.From != nil && !message.From.IsBot && message.SenderChat == nil && message.From.ID == job.ownerID
	if command == "agentstop" && owner {
		job.stopping = true
		job.cancel()
	}
	b.agentMu.Unlock()
	if !b.agentOutputAllowed(&job.message) {
		job.cancel()
		return nil
	}
	if command == "agentstop" {
		if !owner {
			return b.agentReply(ctx, message, "Остановить задачу может только тот, кто её запустил.")
		}
		return b.agentReply(ctx, message, "Останавливаю задачу…")
	}
	if stopping {
		return b.agentReply(ctx, message, "Задача останавливается…")
	}
	return b.agentReply(ctx, message, agentProgressText(progress))
}

func (b *Bot) agentReply(ctx context.Context, message *telegram.Message, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.agentOutputAllowed(message) {
		return nil
	}
	_, err := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, nil)
	return err
}

func (b *Bot) agentFinish(job *agentJob) {
	job.cancel()
	b.agentMu.Lock()
	if b.agentJobs[job.message.Chat.ID] == job {
		delete(b.agentJobs, job.message.Chat.ID)
	}
	b.agentMu.Unlock()
	b.agentWG.Done()
}

func (b *Bot) agentShutdown() {
	b.agentMu.Lock()
	b.agentClosing = true
	for _, job := range b.agentJobs {
		job.cancel()
	}
	b.agentMu.Unlock()
	b.agentWG.Wait()
}

// Permission changes cancel in-flight requests and multi-part replies as well
// as preventing the next tool call. Already delivered messages remain sent.
func (b *Bot) cancelAgentChat(chatID int64) {
	b.agentMu.Lock()
	defer b.agentMu.Unlock()
	if job := b.agentJobs[chatID]; job != nil {
		job.cancel()
	}
}

func (b *Bot) cancelAgentUser(userID int64) {
	b.agentMu.Lock()
	defer b.agentMu.Unlock()
	for _, job := range b.agentJobs {
		if job.ownerID == userID {
			job.cancel()
		}
	}
}
