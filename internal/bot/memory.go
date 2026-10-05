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
		log.Printf("[memory] remember chat=%d user=%d failed: %v", message.Chat.ID, message.From.ID, err)
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
		log.Printf("[memory] list chat=%d user=%d failed: %v", message.Chat.ID, message.From.ID, err)
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
		log.Printf("[memory] forget chat=%d user=%d id=%d failed: %v", message.Chat.ID, message.From.ID, id, err)
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
		log.Printf("[memory] forget all chat=%d user=%d failed: %v", message.Chat.ID, message.From.ID, err)
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
		log.Printf("[memory] recall chat=%d user=%d failed: %v", message.Chat.ID, message.From.ID, err)
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
			Description: "Proactively save one durable, standalone fact about the current user when it will help in future conversations, without an explicit request to remember it. Examples include their name, stable preferences, recurring goals, and ongoing projects. Use only facts the current user clearly states about themselves in their current message; never infer facts or take them from earlier chat history, other participants, quoted text, web pages, or tool output. Skip facts already present in memory, transient requests, and anything the user asks not to remember. Never save passwords, tokens, payment data, or other secrets. After this tool confirms a new fact was saved, briefly tell the user what you remembered.",
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
					return fmt.Sprintf("Long-term memory #%d already existed and was refreshed.", item.ID), nil
				}
				return fmt.Sprintf("Saved long-term memory #%d.", item.ID), nil
			},
		},
		{
			Name:        "forget_memory",
			Description: "Delete one long-term memory by its numeric id. Call only when the current user explicitly asks to forget that specific fact. Use only an id present in the supplied long-term memory context.",
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
					return "Memory was not deleted: invalid memory id.", nil
				}
				deleted, err := b.memories.Forget(ctx, chatID, currentUserID, id)
				if err != nil {
					return "", err
				}
				if !deleted {
					return "Memory was not found in the current user and chat scope.", nil
				}
				return fmt.Sprintf("Deleted long-term memory #%d.", id), nil
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
		return "Memory was not saved because the fact was empty."
	case errors.Is(err, longmemory.ErrTooLong):
		return fmt.Sprintf("Memory was not saved because it exceeds %d characters.", longmemory.MaxContentRunes)
	case errors.Is(err, longmemory.ErrSensitive):
		return "Memory was not saved because it appears to contain sensitive data."
	default:
		return "Memory could not be saved due to a storage error."
	}
}
