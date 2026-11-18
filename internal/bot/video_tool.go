package bot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) videoTool(message *telegram.Message, attachments []cachedAttachment, attachmentErr error) llm.Tool {
	var mu sync.Mutex
	var outcome string
	return llm.Tool{
		Name:        "convert_to_video",
		Description: resources.Get("tool.convert_to_video.description"),
		Parameters: map[string]any{
			"type": "object", "properties": map[string]any{}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, _ map[string]string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if outcome != "" {
				return resources.Format("tool.convert_to_video.already_attempted", map[string]any{"outcome": outcome}), nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			// A lost upload confirmation must not trigger a second delivery.
			if err := b.sendVideo(ctx, message, attachments, attachmentErr); err != nil {
				outcome = resources.Format("tool.convert_to_video.failed", map[string]any{"error": err.Error()})
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
			} else {
				outcome = resources.Get("tool.convert_to_video.sent")
			}
			return outcome, nil
		},
	}
}

func (b *Bot) videoCommand(ctx context.Context, message *telegram.Message) error {
	attachments, collectionErr := b.collectLLMAttachments(ctx, message)
	if err := b.sendVideo(ctx, message, attachments, collectionErr); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID, err.Error(), message.MessageID, nil)
		return errors.Join(err, sendErr)
	}
	return nil
}

func (b *Bot) sendVideo(ctx context.Context, message *telegram.Message, attachments []cachedAttachment, attachmentErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if attachmentErr != nil {
		logError(resources.Get("telegram.log_context.video_attachments"), attachmentErr)
	}
	var target *cachedAttachment
	for i := range attachments {
		attachment := &attachments[i]
		if attachment.Kind == "video" || attachment.Kind == "animation" || attachment.Kind == "video_note" ||
			(attachment.Kind == "photo" && (strings.EqualFold(attachment.MIME, "image/gif") || strings.EqualFold(filepath.Ext(attachment.Name), ".gif"))) {
			target = attachment
			break
		}
	}
	if target == nil {
		if attachmentErr != nil {
			if isFileTooBig(attachmentErr) {
				return errVideoTooLarge
			}
			return attachmentErr
		}
		return errors.New(resources.Get("telegram.video.usage"))
	}
	data, err := b.prepareVideoMP4(ctx, target.Data, target.Name, false)
	if err != nil {
		return fmt.Errorf(resources.Get("telegram.video.convert_failed"), err)
	}
	name := filepath.Base(strings.ReplaceAll(target.Name, "\\", "/"))
	stem := strings.TrimSpace(strings.TrimSuffix(name, filepath.Ext(name)))
	if stem == "" || stem == "." || stem == ".." {
		stem = "video"
	}
	attachment := cachedAttachment{Kind: "video", Name: stem + ".mp4", MIME: "video/mp4", Data: data}
	caption := ""
	if source := llmMediaSource(message); source != nil && source == message.ReplyToMessage {
		caption = source.Caption
	}
	sent, err := b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
		Kind: attachment.Kind, Name: attachment.Name, MIME: attachment.MIME, Data: attachment.Data, Caption: caption,
	}, message.MessageID)
	if err != nil {
		return fmt.Errorf(resources.Get("telegram.video.send_failed"), err)
	}
	if b.media != nil {
		b.media.Put(message.Chat.ID, []telegram.Message{sent}, []cachedAttachment{attachment})
	}
	return nil
}
