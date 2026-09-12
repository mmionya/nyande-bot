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
		Name: "download_media",
		Description: "Download photos or videos from a URL and send the actual files to the current Telegram chat. " +
			"Use when the user asks to find and send a video/photo, or download media. " +
			"If no URL was provided, first use search_tiktok for TikTok requests when available, otherwise web_search, to find a matching media post, then call this tool with its real URL. " +
			"Supports TikTok, Instagram, X/Twitter, Reddit, Xiaohongshu, Pinterest, YouTube and direct image/video URLs. " +
			"Prefer an individual post/video URL; search results pages and ordinary articles cannot be downloaded. " +
			"Never invent a URL or claim media was sent until this tool confirms delivery. " +
			"Only send media requested by the user. The destination is always this chat.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{"url": map[string]any{
				"type": "string", "description": "Actual HTTP(S) media post or direct file URL obtained from the user, web search, or a webpage. Not a search query.",
			}},
			"required": []string{"url"}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]string) (string, error) {
			rawURL, err := mediaToolURL(args["url"])
			if err != nil {
				return "Cannot download media: " + err.Error(), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if outcome, exists := attempts[rawURL]; exists {
				return "This URL was already attempted for this request; no files were sent again. Previous outcome: " + outcome, nil
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
				outcome = fmt.Sprintf("Only %d media files were confirmed sent to this chat; sending the remaining files failed. Do not claim full success or resend this URL.", count)
			case err != nil:
				outcome = "Media delivery was not confirmed: " + humanDownloadError(rawURL, err) + ". Do not claim it was sent. You may find a different source URL."
			case count == 0:
				outcome = "No media files were sent. Find a different direct media or supported post URL."
			default:
				outcome = fmt.Sprintf("Successfully sent %d media files to the current chat in reply to the user's message. Briefly acknowledge delivery; do not send this URL again.", count)
			}
			attempts[rawURL] = outcome
			return outcome, nil
		},
	}
}

func mediaToolURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", errors.New("provide an HTTP(S) URL without embedded credentials")
	}
	u.Fragment = ""
	if isPrivateURL(u.String()) {
		return "", errors.New("local or private network addresses are not allowed")
	}
	if !downloader.Supported(u.String()) {
		return "", errors.New("unsupported page; use a supported media post or a direct JPG, PNG, WebP, GIF, MP4, MOV or WebM file URL")
	}
	return u.String(), nil
}
