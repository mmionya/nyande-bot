package discordbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/resources"
)

// Prefer attachments to links, and the command's own media to replied media.
func gifSource(message *discordgo.Message) string {
	if message == nil {
		return ""
	}
	for _, attachment := range message.Attachments {
		if attachment != nil && isGIFVideoAttachment(attachment) && downloader.Supported(attachment.URL) {
			return attachment.URL
		}
	}
	for _, candidate := range extractURLs(message.Content) {
		if downloader.Supported(candidate) {
			return candidate
		}
	}
	return ""
}

func isGIFVideoAttachment(attachment *discordgo.MessageAttachment) bool {
	if strings.HasPrefix(attachment.ContentType, "video/") || attachment.ContentType == "image/gif" {
		return true
	}
	parsed, err := url.Parse(attachment.URL)
	if err != nil {
		return false
	}
	switch strings.ToLower(filepath.Ext(parsed.Path)) {
	case ".mp4", ".mov", ".webm", ".gif":
		return true
	}
	return false
}

func (b *Bot) handleGIF(session *discordgo.Session, message *discordgo.Message) error {
	ctx, cancel := context.WithTimeout(b.currentContext(), downloadTimeout)
	defer cancel()
	source := gifSource(message)
	if source == "" {
		replied := message.ReferencedMessage
		if replied == nil && message.MessageReference != nil && message.MessageReference.MessageID != "" {
			var err error
			replied, err = session.ChannelMessage(message.ChannelID, message.MessageReference.MessageID, discordgo.WithContext(ctx))
			if err != nil {
				log.Printf("[discord] gif reply lookup failed: %v", err)
				_, sendErr := session.ChannelMessageSendReply(message.ChannelID, resources.Get("discord_gif_reply_failed"), message.SoftReference())
				return sendErr
			}
		}
		source = gifSource(replied)
	}
	if source == "" {
		_, err := session.ChannelMessageSendReply(message.ChannelID, resources.Format("discord_gif_usage", map[string]any{"prefix": b.cfg.DiscordPrefix}), message.SoftReference())
		return err
	}

	b.mediaTotal.Add(1)
	status, err := session.ChannelMessageSendReply(message.ChannelID, resources.Get("discord_downloading"), message.SoftReference())
	if err != nil {
		b.mediaErrors.Add(1)
		return err
	}
	fail := func(text string, cause error) error {
		b.mediaErrors.Add(1)
		log.Printf("[discord] gif failed: %v", cause)
		_, err := session.ChannelMessageEdit(message.ChannelID, status.ID, text)
		return err
	}
	result, err := b.downloader.Download(ctx, source)
	if err != nil {
		return fail(downloadErrorText(err), err)
	}
	var target *downloader.Media
	for i := range result.Items {
		if result.Items[i].Kind == "video" || result.Items[i].Kind == "animation" {
			target = &result.Items[i]
			break
		}
	}
	if target == nil {
		return fail(resources.Get("discord_gif_no_video"), fmt.Errorf("download contained no video"))
	}
	_, _ = session.ChannelMessageEdit(message.ChannelID, status.ID, resources.Get("discord_gif_converting"))
	data, err := convertGIF(ctx, target.Data, b.cfg.MaxFileSize)
	if err != nil {
		key := "discord_gif_failed"
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			key = "discord_gif_timeout"
		} else if errors.Is(err, errGIFTooLarge) {
			key = "discord_gif_too_large"
		}
		return fail(resources.Get(key), err)
	}
	_, _ = session.ChannelMessageEdit(message.ChannelID, status.ID, resources.Get("discord_sending"))
	err = sendResult(session, message, downloader.Result{Items: []downloader.Media{{
		Kind: "animation", Name: resources.Get("discord_gif_filename"), MIME: "image/gif", Data: data,
	}}})
	if err != nil {
		return fail(resources.Get("discord_send_failed"), err)
	}
	return session.ChannelMessageDelete(message.ChannelID, status.ID)
}

var errGIFTooLarge = errors.New("GIF exceeds size limit")

func convertGIF(ctx context.Context, data []byte, maxSize int64) ([]byte, error) {
	if int64(len(data)) > maxSize || maxSize <= 0 {
		return nil, errGIFTooLarge
	}
	dir, err := os.MkdirTemp("", "nyande-discord-gif-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input, output := filepath.Join(dir, "input"), filepath.Join(dir, "animation.gif")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-protocol_whitelist", "file,pipe", "-threads", "1", "-i", input,
		"-filter_complex_threads", "1", "-filter_complex",
		"[0:v:0]fps=15,scale=w='min(480,iw)':h=-1:flags=lanczos,split[a][b];[a]palettegen[p];[b][p]paletteuse",
		"-an", "-loop", "0", "-fs", strconv.FormatInt(maxSize+1, 10), "-y", output)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ffmpeg GIF conversion: %w: %s", err, out)
	}
	info, err := os.Stat(output)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSize {
		return nil, errGIFTooLarge
	}
	return os.ReadFile(output)
}
