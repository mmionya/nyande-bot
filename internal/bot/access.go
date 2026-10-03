package bot

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) blockedMessage(message *telegram.Message) bool {
	// Telegram does not identify the person behind sender_chat. Allowing their
	// requests would let a banned user switch to a channel identity to bypass bans.
	return b.access != nil && (message.SenderChat != nil || message.From == nil ||
		b.access.IsBanned(strconv.FormatInt(message.From.ID, 10)))
}

func (b *Bot) rejectBlockedUpdate(ctx context.Context, update telegram.Update) (bool, error) {
	if query := update.PreCheckoutQuery; query != nil && b.access.IsBanned(strconv.FormatInt(query.From.ID, 10)) {
		return true, b.telegram.AnswerPreCheckout(ctx, query.ID, false, resources.Get("access.blocked"))
	}
	if callback := update.CallbackQuery; callback != nil && b.access.IsBanned(strconv.FormatInt(callback.From.ID, 10)) {
		if callback.Message != nil && b.silentModeration(callback.Message.Chat.ID) {
			return true, nil
		}
		return true, b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("access.blocked"), "", true)
	}
	message := update.Message
	if message == nil {
		message = update.EditedMessage
	}
	if message == nil || !b.blockedMessage(message) {
		return false, nil
	}
	// Bans stop bot services, but must not exempt a user from link moderation.
	if b.linkDeletionEnabled(message.Chat.ID) {
		if urls := extractURLs(message); len(urls) > 0 {
			return true, b.handleLinks(ctx, message, urls)
		}
	}
	return true, nil
}

func (b *Bot) botBanCommand(ctx context.Context, message *telegram.Message, command, arguments string) error {
	token := strings.Fields(message.ContentText())[0]
	if _, username, addressed := strings.Cut(token, "@"); addressed && !strings.EqualFold(username, b.identity.Username) {
		return nil
	}
	if message.ForwardOrigin != nil || message.IsAutomaticForward {
		return nil
	}
	respond := func(key string) error {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
		return err
	}
	if message.SenderChat != nil || message.From == nil || !b.access.IsOwner(strconv.FormatInt(message.From.ID, 10)) {
		return respond("access.owner_only")
	}
	// Ban commands accept exactly the same unambiguous reply/ID targets as link permissions.
	target, err := linkPermissionTarget(message, arguments)
	if err != nil {
		return respond("access.telegram_usage")
	}
	id := strconv.FormatInt(target, 10)
	if target == b.identity.ID || b.access.IsOwner(id) {
		return respond("access.protected")
	}
	banned := command == "botban"
	if err := b.access.SetBanned(id, banned); err != nil {
		return errors.Join(err, respond("access.save_failed"))
	}
	log.Printf("[access] platform=telegram owner=%d target=%d banned=%t", message.From.ID, target, banned)
	key := "access.unbanned"
	if banned {
		key = "access.banned"
	}
	_, err = b.telegram.SendMessage(ctx, message.Chat.ID, resources.Format(key, map[string]any{"id": id}), message.MessageID, nil)
	return err
}
