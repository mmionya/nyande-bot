package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

// Permission commands are the only commands executed by silent moderation.
// They never send a response, invoke the LLM or modify domain allowlists.
func (b *Bot) handleLinkPermissionCommand(ctx context.Context, message *telegram.Message) (bool, error) {
	if !b.silentModeration(message.Chat.ID) || message.IsAutomaticForward || message.ForwardOrigin != nil {
		return false, nil
	}
	command, args, ok := parseCommand(strings.TrimSpace(message.ContentText()), b.identity.Username)
	if !ok || (command != "permitlinks" && command != "revokelinks") {
		return false, nil
	}
	unlockMedia := b.lockUpdates(message.Chat.ID, true)
	defer unlockMedia()
	admin, err := b.isAdmin(ctx, message)
	if err != nil {
		return true, fmt.Errorf(resources.Get("telegram.error.check_link_admin"), err)
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
		return true, errors.New(resources.Get("telegram.error.link_permission_bot"))
	}
	permitted := command == "permitlinks"
	if err := b.linkConfig.SetUserPermission(message.Chat.ID, target, permitted); err != nil {
		return true, fmt.Errorf(resources.Get("telegram.error.save_link_permission"), err)
	}
	senderChat := int64(0)
	if message.SenderChat != nil {
		senderChat = message.SenderChat.ID
	}
	log.Printf(resources.Get("telegram.log.link_permission"), message.Chat.ID, userID(message), senderChat, target, permitted, message.MessageID)
	return true, nil
}

func linkPermissionTarget(message *telegram.Message, args string) (int64, error) {
	if args != "" {
		if message.ReplyToMessage != nil {
			return 0, errors.New(resources.Get("telegram.error.link_permission_target_ambiguous"))
		}
		id, err := strconv.ParseInt(args, 10, 64)
		if err != nil || id <= 0 {
			return 0, errors.New(resources.Get("telegram.error.link_permission_target_invalid"))
		}
		return id, nil
	}
	reply := message.ReplyToMessage
	if reply == nil || reply.Chat.ID != message.Chat.ID || reply.SenderChat != nil || reply.From == nil || reply.From.ID <= 0 || reply.From.IsBot {
		return 0, errors.New(resources.Get("telegram.error.link_permission_reply_required"))
	}
	return reply.From.ID, nil
}
