package bot

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) downloadMediaTool(message *telegram.Message) llm.Tool {
	return newDownloadMediaTool(func(ctx context.Context, rawURL string) (int, error) {
		sent, err := b.downloadAndSendMedia(ctx, message, rawURL)
		return len(sent), err
	})
}

func newDownloadMediaTool(deliver func(context.Context, string) (int, error)) llm.Tool {
	// A new tool is created for each user request. Remember attempts, including
	// ambiguous/partial Telegram failures, to avoid sending the same files twice.
	var mu sync.Mutex
	attempts := make(map[string]string)
	return llm.Tool{
		Name:        "download_media",
		Description: resources.Get("tool.download_media.description"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{"url": map[string]any{
				"type": "string", "description": resources.Get("tool.download_media.url_description"),
			}},
			"required": []string{"url"}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]string) (string, error) {
			rawURL, err := mediaToolURL(args["url"])
			if err != nil {
				return resources.Format("tool.download_media.invalid_url", map[string]any{"error": err.Error()}), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if outcome, exists := attempts[rawURL]; exists {
				return resources.Format("tool.download_media.already_attempted", map[string]any{"outcome": outcome}), nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			downloadCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			count, err := deliver(downloadCtx, rawURL)
			var outcome string
			switch {
			case err != nil && count > 0:
				outcome = fmt.Sprintf(resources.Get("tool.download_media.partial"), count)
			case err != nil:
				outcome = resources.Format("tool.download_media.delivery_unconfirmed", map[string]any{"error": humanDownloadError(rawURL, err)})
			case count == 0:
				outcome = resources.Get("tool.download_media.empty")
			default:
				outcome = fmt.Sprintf(resources.Get("tool.download_media.sent"), count)
			}
			attempts[rawURL] = outcome
			return outcome, nil
		},
	}
}

func mediaToolURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", errors.New(resources.Get("tool.download_media.url_required"))
	}
	u.Fragment = ""
	if isPrivateURL(u.String()) {
		return "", errors.New(resources.Get("tool.download_media.private_url"))
	}
	if !downloader.Supported(u.String()) {
		return "", errors.New(resources.Get("tool.download_media.unsupported"))
	}
	return u.String(), nil
}
