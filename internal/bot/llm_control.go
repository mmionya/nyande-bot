package bot

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) llmDisabled(chatID int64) bool {
	return b.llmConfig.Disabled(chatID)
}

func (b *Bot) llmStatus() string {
	if b.llm == nil || !b.llm.Enabled() {
		if b.chatlog == nil {
			return "llm.control.global_disabled"
		}
		return "llm.control.global_disabled_collecting"
	}
	if b.chatlog == nil {
		return "llm.control.enabled_no_archive"
	}
	return "llm.control.enabled"
}

func (b *Bot) configureLLM(ctx context.Context, message *telegram.Message, argument string) error {
	respond := func(key string) error {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
		return err
	}
	admin, err := b.isAdmin(ctx, message)
	if err != nil {
		return err
	}
	if !admin {
		return respond("llm.control.admin_only")
	}
	value := strings.ToLower(strings.TrimSpace(argument))
	if value == "" {
		if b.llmDisabled(message.Chat.ID) {
			return respond("llm.control.disabled")
		}
		return respond(b.llmStatus())
	}
	if value != "on" && value != "off" {
		return respond("llm.control.usage")
	}
	if b.llmConfig == nil {
		return respond("llm.control.save_failed")
	}
	disabled := value == "off"
	if err := b.llmConfig.SetDisabled(message.Chat.ID, disabled); err != nil {
		return errors.Join(err, respond("llm.control.save_failed"))
	}
	log.Printf(resources.Get("llm.control.log_changed"), message.Chat.ID, !disabled)
	if disabled {
		b.cancelAgentChat(message.Chat.ID)
		return respond("llm.control.disabled")
	}
	return respond(b.llmStatus())
}
