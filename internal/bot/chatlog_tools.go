package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) chatlogTools(message *telegram.Message) []llm.Tool {
	if b.chatlog == nil || message == nil {
		return nil
	}
	chatID := message.Chat.ID
	return []llm.Tool{
		{
			Name:        "search_chat_messages",
			Description: resources.Get("tool.chat_search.description"),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": resources.Get("tool.chat_search.query_description"),
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				query := strings.TrimSpace(arguments["query"])
				if query == "" {
					return resources.Get("tool.chat_search.empty_query"), nil
				}
				messages, err := b.chatlog.Search(ctx, chatID, query, 15)
				if err != nil {
					return fmt.Sprintf(resources.Get("tool.chat_search.failed"), err), nil
				}
				if len(messages) == 0 {
					return fmt.Sprintf(resources.Get("tool.chat_search.no_results"), query), nil
				}
				var bld strings.Builder
				fmt.Fprintf(&bld, resources.Get("tool.chat_search.results"), len(messages))
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
			Name:        "get_recent_messages",
			Description: resources.Get("tool.chat_recent.description"),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": resources.Get("tool.chat_recent.limit_description"),
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
					return fmt.Sprintf(resources.Get("tool.chat_recent.failed"), err), nil
				}
				if len(messages) == 0 {
					return resources.Get("tool.chat_recent.empty"), nil
				}
				var bld strings.Builder
				fmt.Fprintf(&bld, resources.Get("tool.chat_recent.results"), len(messages))
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
