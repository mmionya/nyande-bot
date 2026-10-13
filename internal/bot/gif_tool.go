package bot

import (
	"context"
	"sync"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) gifTool(message *telegram.Message, attachments []cachedAttachment, attachmentErr error) llm.Tool {
	var mu sync.Mutex
	var outcome string
	return llm.Tool{
		Name:        "convert_to_gif",
		Description: resources.Get("tool.convert_to_gif.description"),
		Parameters: map[string]any{
			"type": "object", "properties": map[string]any{}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, _ map[string]string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if outcome != "" {
				return resources.Format("tool.convert_to_gif.already_attempted", map[string]any{"outcome": outcome}), nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			// Reuse the attachments already collected for this LLM turn. Record
			// failures too: an ambiguous upload must not send duplicate animations.
			if err := b.sendGIF(ctx, message, attachments, attachmentErr); err != nil {
				outcome = resources.Format("tool.convert_to_gif.failed", map[string]any{"error": err.Error()})
			} else {
				outcome = resources.Get("tool.convert_to_gif.sent")
			}
			return outcome, nil
		},
	}
}
