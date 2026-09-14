package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

// Permission commands are the only commands executed by silent moderation.
// They never send a response, invoke the LLM or modify domain allowlists.
func (b *Bot) handleLinkPermissionCommand(ctx context.Context, message *telegram.Message) (bool, error) {
	if !b.silentModeration(message.Chat.ID) || message.IsAutomaticForward || message.ForwardOrigin != nil {
		return false, nil
	}
	command, args, ok := parseCommand(strings.TrimSpace(message.ContentText()))
	if !ok || (command != "permitlinks" && command != "revokelinks") {
		return false, nil
	}
	token := strings.Fields(message.ContentText())[0]
	if _, username, addressed := strings.Cut(token, "@"); addressed && (b.identity.Username == "" || !strings.EqualFold(username, b.identity.Username)) {
		return false, nil
	}
	admin, err := b.isAdmin(ctx, message)
	if err != nil {
		return true, fmt.Errorf("check link permission administrator: %w", err)
	}
	// Non-admin commands still undergo link deletion; a command prefix must not
	// allow someone to sneak a link past moderation.
	if !admin {
		return false, nil
	}
	target, err := linkPermissionTarget(message, args)
	if err != nil {
		return true, err
	}
	if target == b.identity.ID {
		return true, errors.New("cannot grant link permission to this bot")
	}
	permitted := command == "permitlinks"
	if err := b.linkConfig.SetUserPermission(message.Chat.ID, target, permitted); err != nil {
		return true, fmt.Errorf("save link permission: %w", err)
	}
	senderChat := int64(0)
	if message.SenderChat != nil {
		senderChat = message.SenderChat.ID
	}
	log.Printf("[moderation] link_permission platform=telegram chat=%d admin=%d sender_chat=%d target=%d allowed=%t msg=%d", message.Chat.ID, userID(message), senderChat, target, permitted, message.MessageID)
	return true, nil
}

func linkPermissionTarget(message *telegram.Message, args string) (int64, error) {
	if args != "" {
		if message.ReplyToMessage != nil {
			return 0, errors.New("use either a reply or a numeric user ID for link permission")
		}
		id, err := strconv.ParseInt(args, 10, 64)
		if err != nil || id <= 0 {
			return 0, errors.New("link permission requires a reply to a user or a positive numeric user ID")
		}
		return id, nil
	}
	reply := message.ReplyToMessage
	if reply == nil || reply.Chat.ID != message.Chat.ID || reply.SenderChat != nil || reply.From == nil || reply.From.ID <= 0 || reply.From.IsBot {
		return 0, errors.New("reply to a participant's message to grant or revoke link permission")
	}
	return reply.From.ID, nil
}
