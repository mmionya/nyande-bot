package bot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/internal/webpage"
	"github.com/mmionya/nyande-bot/resources"
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
			Name:        "set_reminder",
			Description: resources.Get("tool.set_reminder.description"),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": resources.Get("tool.set_reminder.text_description"),
					},
					"delay_seconds": map[string]any{
						"type":        "integer",
						"description": resources.Get("tool.set_reminder.delay_description"),
						"minimum":     5,
					},
				},
				"required":             []string{"text", "delay_seconds"},
				"additionalProperties": false,
			},
			Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
				text := strings.TrimSpace(arguments["text"])
				if text == "" {
					return resources.Get("tool.set_reminder.empty_text"), nil
				}
				delay, err := parseDelaySeconds(arguments["delay_seconds"])
				if err != nil {
					return fmt.Sprintf(resources.Get("tool.set_reminder.invalid_delay"), err), nil
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
					return fmt.Sprintf(resources.Get("tool.set_reminder.save_failed"), err), nil
				}
				durStr := formatHumanDuration(time.Duration(delay) * time.Second)
				return fmt.Sprintf(resources.Get("tool.set_reminder.saved"), id, text, durStr), nil
			},
		})
	}

	tools = append(tools, llm.Tool{
		Name:        "read_webpage",
		Description: resources.Get("tool.read_webpage.description"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": resources.Get("tool.read_webpage.url_description"),
				},
			},
			"required":             []string{"url"},
			"additionalProperties": false,
		},
		Execute: func(ctx context.Context, arguments map[string]string) (string, error) {
			rawURL := strings.TrimSpace(arguments["url"])
			if rawURL == "" {
				return resources.Get("tool.read_webpage.empty_url"), nil
			}
			if isPrivateURL(rawURL) {
				return resources.Get("tool.read_webpage.private_url"), nil
			}
			page, err := webpage.Fetch(ctx, rawURL, 6000)
			if err != nil {
				return fmt.Sprintf(resources.Get("tool.read_webpage.failed"), rawURL, err), nil
			}
			var bld strings.Builder
			if page.Title != "" {
				fmt.Fprintf(&bld, resources.Get("tool.read_webpage.title"), page.Title)
			}
			fmt.Fprintf(&bld, resources.Get("tool.read_webpage.content"), page.URL, page.Content)
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
		return fmt.Sprintf(resources.Get("telegram.duration.hours_minutes"), h, m)
	}
	if m > 0 {
		if s > 0 {
			return fmt.Sprintf(resources.Get("telegram.duration.minutes_seconds"), m, s)
		}
		return fmt.Sprintf(resources.Get("telegram.duration.minutes"), m)
	}
	return fmt.Sprintf(resources.Get("telegram.duration.seconds"), s)
}

func parseDelaySeconds(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New(resources.Get("tool.set_reminder.delay_empty"))
	}
	if val, err := strconv.ParseFloat(raw, 64); err == nil {
		if val < 5 {
			return 0, errors.New(resources.Get("tool.set_reminder.delay_too_short"))
		}
		if val > 365*24*3600 {
			return 0, errors.New(resources.Get("tool.set_reminder.delay_too_long"))
		}
		return int(val), nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		sec := int(d.Seconds())
		if sec < 5 {
			return 0, errors.New(resources.Get("tool.set_reminder.delay_too_short"))
		}
		if sec > 365*24*3600 {
			return 0, errors.New(resources.Get("tool.set_reminder.delay_too_long"))
		}
		return sec, nil
	}
	return 0, fmt.Errorf(resources.Get("tool.set_reminder.delay_unrecognized"), raw)
}

func isPrivateURL(rawURL string) bool {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	hostname := parsed.Hostname()
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	if ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	return false
}
