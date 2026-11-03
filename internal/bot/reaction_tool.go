package bot

import (
	"context"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func (b *Bot) reactionTool(message *telegram.Message) llm.Tool {
	chatID, messageID, replyID := message.Chat.ID, message.MessageID, 0
	if reply := message.ReplyToMessage; reply != nil && (reply.Chat.ID == 0 || reply.Chat.ID == chatID) {
		replyID = reply.MessageID
	}
	return llm.ReactionTool(replyID > 0, func(ctx context.Context, target, emoji string) error {
		id := messageID
		if target == "reply" {
			id = replyID
		}
		return b.telegram.SetMessageReaction(ctx, chatID, id, emoji)
	})
}
