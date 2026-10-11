package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/llm"
	longmemory "github.com/mmionya/nyande-bot/internal/memory"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) rememberCommand(ctx context.Context, message *telegram.Message, content string) error {
	if !b.memoryAvailable(message) {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.unavailable"))
	}
	if strings.TrimSpace(content) == "" {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.remember.usage"))
	}
	item, created, err := b.memories.Remember(ctx, message.Chat.ID, message.From.ID, message.From.DisplayName(), content)
	if err != nil {
		log.Printf(resources.Get("telegram.log.memory_remember_failed"), message.Chat.ID, message.From.ID, err)
		return b.sendMemoryMessage(ctx, message, memoryCommandError(err))
	}
	key := "memory.remember.exists"
	if created {
		key = "memory.remember.done"
	}
	return b.sendMemoryMessage(ctx, message, resources.Format(key, map[string]any{"id": item.ID}))
}

func (b *Bot) memoryCommand(ctx context.Context, message *telegram.Message) error {
	if !b.memoryAvailable(message) {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.unavailable"))
	}
	items, err := b.memories.List(ctx, message.Chat.ID, message.From.ID, b.cfg.LLMMemoryMax)
	if err != nil {
		log.Printf(resources.Get("telegram.log.memory_list_failed"), message.Chat.ID, message.From.ID, err)
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.error"))
	}
	if len(items) == 0 {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.list.empty"))
	}
	var text strings.Builder
	text.WriteString(resources.Get("memory.list.header"))
	for _, item := range items {
		fmt.Fprintf(&text, "\n#%d: %s", item.ID, item.Content)
	}
	return b.sendLLMAnswer(ctx, message, text.String())
}

func (b *Bot) forgetCommand(ctx context.Context, message *telegram.Message, argument string) error {
	if !b.memoryAvailable(message) {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.unavailable"))
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(argument), "#"), 10, 64)
	if err != nil || id < 1 {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.forget.usage"))
	}
	deleted, err := b.memories.Forget(ctx, message.Chat.ID, message.From.ID, id)
	if err != nil {
		log.Printf(resources.Get("telegram.log.memory_forget_failed"), message.Chat.ID, message.From.ID, id, err)
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.error"))
	}
	key := "memory.forget.missing"
	if deleted {
		key = "memory.forget.done"
	}
	return b.sendMemoryMessage(ctx, message, resources.Format(key, map[string]any{"id": id}))
}

func (b *Bot) forgetAllCommand(ctx context.Context, message *telegram.Message) error {
	if !b.memoryAvailable(message) {
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.unavailable"))
	}
	count, err := b.memories.ForgetAll(ctx, message.Chat.ID, message.From.ID)
	if err != nil {
		log.Printf(resources.Get("telegram.log.memory_forget_all_failed"), message.Chat.ID, message.From.ID, err)
		return b.sendMemoryMessage(ctx, message, resources.Get("memory.error"))
	}
	return b.sendMemoryMessage(ctx, message, resources.Format("memory.forget_all.done", map[string]any{"count": count}))
}

func (b *Bot) recallMemories(ctx context.Context, message *telegram.Message, query string) []llm.Memory {
	if !b.memoryAvailable(message) {
		return nil
	}
	items, err := b.memories.Recall(ctx, message.Chat.ID, message.From.ID, query, b.cfg.LLMMemoryRecall)
	if err != nil {
		log.Printf(resources.Get("telegram.log.memory_recall_failed"), message.Chat.ID, message.From.ID, err)
		return nil
	}
	result := make([]llm.Memory, 0, len(items))
	for _, item := range items {
		result = append(result, llm.Memory{ID: item.ID, Content: item.Content})
	}
	return result
}

func (b *Bot) memoryTools(message *telegram.Message) []llm.Tool {
	if !b.memoryAvailable(message) {
		return nil
	}
	chatID, currentUserID := message.Chat.ID, message.From.ID
	userName := message.From.DisplayName()
	return []llm.Tool{
		{
			Name:        "remember_memory",
			Description: resources.Get("tool.remember_fact.description"),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fact": map[string]any{"type": "string", "maxLength": longmemory.MaxContentRunes},
				},
				"required": []string{"fact"}, "additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				item, created, err := b.memories.Remember(ctx, chatID, currentUserID, userName, arguments["fact"])
				if err != nil {
					return memoryToolError(err), nil
				}
				if !created {
					return fmt.Sprintf(resources.Get("tool.remember_fact.refreshed"), item.ID), nil
				}
				return fmt.Sprintf(resources.Get("tool.remember_fact.saved"), item.ID), nil
			},
		},
		{
			Name:        "forget_memory",
			Description: resources.Get("tool.forget_fact.description"),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_id": map[string]any{"type": "integer", "minimum": 1},
				},
				"required": []string{"memory_id"}, "additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				id, err := strconv.ParseInt(strings.TrimSpace(arguments["memory_id"]), 10, 64)
				if err != nil || id < 1 {
					return resources.Get("tool.forget_fact.invalid_id"), nil
				}
				deleted, err := b.memories.Forget(ctx, chatID, currentUserID, id)
				if err != nil {
					return "", err
				}
				if !deleted {
					return resources.Get("tool.forget_fact.not_found"), nil
				}
				return fmt.Sprintf(resources.Get("tool.forget_fact.deleted"), id), nil
			},
		},
	}
}

func (b *Bot) memoryAvailable(message *telegram.Message) bool {
	return b.memories != nil && message != nil && message.From != nil && message.From.ID != 0
}

func (b *Bot) sendMemoryMessage(ctx context.Context, message *telegram.Message, text string) error {
	_, err := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, nil)
	return err
}

func memoryCommandError(err error) string {
	switch {
	case errors.Is(err, longmemory.ErrEmpty):
		return resources.Get("memory.remember.usage")
	case errors.Is(err, longmemory.ErrTooLong):
		return resources.Format("memory.remember.too_long", map[string]any{"maximum": longmemory.MaxContentRunes})
	case errors.Is(err, longmemory.ErrSensitive):
		return resources.Get("memory.remember.sensitive")
	default:
		return resources.Get("memory.error")
	}
}

func memoryToolError(err error) string {
	switch {
	case errors.Is(err, longmemory.ErrEmpty):
		return resources.Get("tool.remember_fact.empty")
	case errors.Is(err, longmemory.ErrTooLong):
		return fmt.Sprintf(resources.Get("tool.remember_fact.too_long"), longmemory.MaxContentRunes)
	case errors.Is(err, longmemory.ErrSensitive):
		return resources.Get("tool.remember_fact.sensitive")
	default:
		return resources.Get("tool.remember_fact.storage_error")
	}
}
