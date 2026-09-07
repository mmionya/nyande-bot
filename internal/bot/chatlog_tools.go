package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func (b *Bot) chatlogTools(message *telegram.Message) []llm.Tool {
	if b.chatlog == nil || message == nil {
		return nil
	}
	chatID := message.Chat.ID
	return []llm.Tool{
		{
			Name: "search_chat_messages",
			Description: "Search historical messages sent by users in this chat by keyword or phrase. " +
				"Use when the user asks about past topics, who said what, earlier discussions, or requests to find a previous message.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Keywords or phrase to search for in the chat history",
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				query := strings.TrimSpace(arguments["query"])
				if query == "" {
					return "Please provide a non-empty search query.", nil
				}
				messages, err := b.chatlog.Search(ctx, chatID, query, 15)
				if err != nil {
					return fmt.Sprintf("Error searching chat messages: %v", err), nil
				}
				if len(messages) == 0 {
					return fmt.Sprintf("No messages found matching %q in this chat.", query), nil
				}
				var bld strings.Builder
				fmt.Fprintf(&bld, "Found %d messages in chat history:\n", len(messages))
				for _, m := range messages {
					author := m.DisplayName
					if m.Username != "" {
						author += " (@" + m.Username + ")"
					}
					timeStr := m.Date.Format("2006-01-02 15:04:05")
					fmt.Fprintf(&bld, "- [%s] %s: %s\n", timeStr, author, m.Text)
				}
				return bld.String(), nil
			},
		},
		{
			Name: "get_recent_messages",
			Description: "Get the most recent messages sent in this chat in chronological order (oldest to newest). " +
				"Use when the user asks what people were just discussing, asks for a summary of recent chat activity, or references recent conversation context.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Number of recent messages to retrieve (1-30, default 15)",
						"minimum":     1,
						"maximum":     30,
					},
				},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				limit := 15
				if val, ok := arguments["limit"]; ok {
					cleaned := strings.TrimSpace(val)
					if parsed, err := strconv.Atoi(cleaned); err == nil && parsed > 0 {
						limit = parsed
					} else if f, err := strconv.ParseFloat(cleaned, 64); err == nil && f > 0 {
						limit = int(f)
					}
				}
				if limit > 30 {
					limit = 30
				}
				messages, err := b.chatlog.Recent(ctx, chatID, limit)
				if err != nil {
					return fmt.Sprintf("Error retrieving recent messages: %v", err), nil
				}
				if len(messages) == 0 {
					return "No recent messages found in this chat.", nil
				}
				var bld strings.Builder
				fmt.Fprintf(&bld, "Recent %d messages in this chat (chronological order):\n", len(messages))
				for _, m := range messages {
					author := m.DisplayName
					if m.Username != "" {
						author += " (@" + m.Username + ")"
					}
					timeStr := m.Date.Format("2006-01-02 15:04:05")
					fmt.Fprintf(&bld, "- [%s] %s: %s\n", timeStr, author, m.Text)
				}
				return bld.String(), nil
			},
		},
	}
}
