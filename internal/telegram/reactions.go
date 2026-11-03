package telegram

import (
	"context"
	"strings"
)

// SetMessageReaction sets the bot's single standard emoji reaction.
func (c *Client) SetMessageReaction(ctx context.Context, chatID int64, messageID int, emoji string) error {
	return c.call(ctx, "setMessageReaction", map[string]any{
		"chat_id": chatID, "message_id": messageID,
		"reaction": []map[string]string{{"type": "emoji", "emoji": strings.ReplaceAll(emoji, "\ufe0f", "")}},
	}, nil)
}
