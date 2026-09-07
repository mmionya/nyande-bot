package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/internal/webpage"
)

func (b *Bot) reminderTools(message *telegram.Message) []llm.Tool {
	var tools []llm.Tool

	if b.reminders != nil && message != nil {
		chatID := message.Chat.ID
		userIDVal := userID(message)
		userName := ""
		if message.From != nil {
			userName = message.From.DisplayName()
		}
		msgID := message.MessageID

		tools = append(tools, llm.Tool{
			Name: "set_reminder",
			Description: "Set a timed reminder for the user or chat. " +
				"Use when the user asks to be reminded about something after a delay (e.g. 'напомни через 15 минут попить воды') or at a specific time. " +
				"Takes the reminder text and delay in seconds from current time.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "What to remind the user about (the reminder note)",
					},
					"delay_seconds": map[string]any{
						"type":        "integer",
						"description": "Number of seconds from now until the reminder should trigger (e.g., 60 for 1 minute, 1800 for 30 minutes, 3600 for 1 hour)",
						"minimum":     5,
					},
				},
				"required":             []string{"text", "delay_seconds"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				text := strings.TrimSpace(arguments["text"])
				if text == "" {
					return "Reminder text cannot be empty.", nil
				}
				delay, err := strconv.Atoi(arguments["delay_seconds"])
				if err != nil || delay < 5 {
					return "Invalid delay_seconds. It must be at least 5 seconds.", nil
				}
				triggerAt := time.Now().UTC().Add(time.Duration(delay) * time.Second)
				id, err := b.reminders.Save(ctx, reminders.Reminder{
					ChatID:    chatID,
					UserID:    userIDVal,
					UserName:  userName,
					MessageID: msgID,
					Text:      text,
					TriggerAt: triggerAt,
					CreatedAt: time.Now().UTC(),
				})
				if err != nil {
					return fmt.Sprintf("Failed to save reminder: %v", err), nil
				}
				durStr := formatHumanDuration(time.Duration(delay) * time.Second)
				return fmt.Sprintf("Reminder #%d set successfully! I will remind the user about %q in %s.", id, text, durStr), nil
			},
		})
	}

	tools = append(tools, llm.Tool{
		Name: "read_webpage",
		Description: "Fetch and read the text content of a web page URL. " +
			"Use when the user provides a link and asks to summarize, analyze, read, or answer questions about what is written on that web page.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The HTTP or HTTPS URL of the web page to read",
				},
			},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
			rawURL := strings.TrimSpace(arguments["url"])
			if rawURL == "" {
				return "Please provide a valid web page URL.", nil
			}
			page, err := webpage.Fetch(ctx, rawURL, 6000)
			if err != nil {
				return fmt.Sprintf("Failed to read webpage %s: %v", rawURL, err), nil
			}
			var bld strings.Builder
			if page.Title != "" {
				fmt.Fprintf(&bld, "Page Title: %s\n", page.Title)
			}
			fmt.Fprintf(&bld, "URL: %s\n\nContent:\n%s", page.URL, page.Content)
			return bld.String(), nil
		},
	})

	return tools
}

func formatHumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	h := m / 60
	m = m % 60
	if h > 0 {
		return fmt.Sprintf("%d ч %d мин", h, m)
	}
	if m > 0 {
		if s > 0 {
			return fmt.Sprintf("%d мин %d сек", m, s)
		}
		return fmt.Sprintf("%d мин", m)
	}
	return fmt.Sprintf("%d сек", s)
}
