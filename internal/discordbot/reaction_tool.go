package discordbot

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/llm"
)

func reactionTool(session *discordgo.Session, message *discordgo.Message) llm.Tool {
	channelID, messageID, replyID := message.ChannelID, message.ID, ""
	ref := message.MessageReference
	if ref == nil || ref.Type == discordgo.MessageReferenceTypeDefault {
		if reply := message.ReferencedMessage; reply != nil && (reply.ChannelID == "" || reply.ChannelID == channelID) {
			replyID = reply.ID
		} else if ref != nil && (ref.ChannelID == "" || ref.ChannelID == channelID) {
			replyID = ref.MessageID
		}
	}
	return llm.ReactionTool(replyID != "", func(ctx context.Context, target, emoji string) error {
		id := messageID
		if target == "reply" {
			id = replyID
		}
		return session.MessageReactionAdd(channelID, id, emoji, discordgo.WithContext(ctx))
	})
}
