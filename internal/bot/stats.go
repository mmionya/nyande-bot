package bot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

type statsSnapshot struct {
	Uptime      time.Duration
	Chats       int64
	Messages    int64
	Commands    int64
	Media       int64
	MediaErrors int64
	LLMCalls    int64
	LLMErrors   int64
}

// Each supersampled canvas uses about 23 MiB. Bound concurrent renders across
// chats independently of update workers; release the slot before uploading.
var statsRenderSlots = make(chan struct{}, 2)

func (b *Bot) statsSnapshot() statsSnapshot {
	stats := statsSnapshot{
		Uptime:      max(0, time.Since(b.state.StartedAt).Round(time.Second)),
		Chats:       int64(b.state.UniqueChats()),
		Messages:    max(0, b.state.MessagesTotal.Load()),
		Commands:    max(0, b.state.CommandsUsed.Load()),
		Media:       max(0, b.state.MediaTotal.Load()),
		MediaErrors: max(0, b.state.MediaErrors.Load()),
		LLMCalls:    max(0, b.state.LLMCalls.Load()),
		LLMErrors:   max(0, b.state.LLMErrors.Load()),
	}
	// The counters are atomic individually, so another chat can update them
	// between reads. Keep ratios and derived success counts consistent.
	if stats.Commands > stats.Messages {
		stats.Commands = stats.Messages
	}
	if stats.MediaErrors > stats.Media {
		stats.MediaErrors = stats.Media
	}
	if stats.LLMErrors > stats.LLMCalls {
		stats.LLMErrors = stats.LLMCalls
	}
	return stats
}

func (b *Bot) statsCommand(ctx context.Context, message *telegram.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stats := b.statsSnapshot()
	select {
	case statsRenderSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	data, err := func() ([]byte, error) {
		defer func() { <-statsRenderSlots }()
		return renderStatsCard(stats)
	}()
	if err == nil {
		_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
			Kind: "photo", Name: "stats.png", MIME: "image/png", Data: data,
		}, message.MessageID)
		if err == nil || !mediaUploadRejected(err) {
			return err
		}
		log.Printf(resources.Get("telegram.log.stats_photo_rejected"), err)
	} else {
		log.Printf(resources.Get("telegram.log.stats_render_failed"), err)
	}
	_, err = b.telegram.SendMessage(ctx, message.Chat.ID, stats.text(), message.MessageID, nil)
	return err
}

func (stats statsSnapshot) text() string {
	errorRate := float64(0)
	if stats.Media > 0 {
		errorRate = float64(stats.MediaErrors) / float64(stats.Media) * 100
	}
	return resources.Format("bot.stats", map[string]any{
		"uptime": stats.Uptime, "chats": stats.Chats,
		"messages": stats.Messages, "commands": stats.Commands,
		"media": stats.Media, "success": stats.Media - stats.MediaErrors, "errors": stats.MediaErrors,
		"error_rate": fmt.Sprintf("%.1f", errorRate),
		"llm_calls":  stats.LLMCalls, "llm_errors": stats.LLMErrors,
	})
}
