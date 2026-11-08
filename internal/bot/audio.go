package bot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

var errAudioTooLarge = errors.New(resources.Get("telegram.audio.too_large"))

func (b *Bot) audioFileLimit() int64 {
	const telegramLimit = 50 * 1024 * 1024
	if b.cfg.MaxFileSize > 0 && b.cfg.MaxFileSize < telegramLimit {
		return b.cfg.MaxFileSize
	}
	return telegramLimit
}

func (b *Bot) convertMediaToAudio(ctx context.Context, data []byte, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := b.audioFileLimit()
	if int64(len(data)) > limit {
		return nil, errAudioTooLarge
	}
	tempDir, err := os.MkdirTemp("", "nyande-audio-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	input := filepath.Join(tempDir, "input"+safeExtension(name, ".bin"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}

	execCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	output := filepath.Join(tempDir, "audio.mp3")
	cmd := exec.CommandContext(execCtx, firstString(b.cfg.FFmpegPath, "ffmpeg"),
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", input, "-map", "0:a:0", "-vn",
		"-c:a", "libmp3lame", "-b:a", "192k", "-ar", "44100", "-ac", "2",
		"-fs", fmt.Sprint(limit+1), "-y", output,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if execCtx.Err() != nil {
			return nil, execCtx.Err()
		}
		return nil, fmt.Errorf(resources.Get("telegram.error.audio_conversion"), err, strings.TrimSpace(string(out)))
	}
	info, err := os.Stat(output)
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, errAudioTooLarge
	}
	return os.ReadFile(output)
}

func (b *Bot) audioCommand(ctx context.Context, message *telegram.Message) error {
	attachments, collectionErr := b.collectLLMAttachments(ctx, message)
	if collectionErr != nil {
		logError(resources.Get("telegram.log_context.audio_attachments"), collectionErr)
	}
	var target *cachedAttachment
	for i := range attachments {
		switch attachments[i].Kind {
		case "audio", "voice", "video", "animation", "video_note":
			target = &attachments[i]
		}
		if target != nil {
			break
		}
	}
	if target == nil {
		key := "telegram.audio.usage"
		if collectionErr != nil {
			key = "telegram.media.download_failed"
			if isFileTooBig(collectionErr) {
				key = "telegram.media.attachment_too_large"
			}
		}
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
		return err
	}

	audio, err := b.convertMediaToAudio(ctx, target.Data, target.Name)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		text := fmt.Sprintf(resources.Get("telegram.audio.convert_failed"), err)
		if errors.Is(err, errAudioTooLarge) {
			text = resources.Get("telegram.audio.too_large")
		}
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, nil)
		return sendErr
	}

	name := filepath.Base(strings.ReplaceAll(target.Name, "\\", "/"))
	stem := strings.TrimSpace(strings.TrimSuffix(name, filepath.Ext(name)))
	if stem == "" || stem == "." || stem == ".." {
		stem = "audio"
	}
	_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
		Kind: "audio", Name: stem + ".mp3", MIME: "audio/mpeg", Data: audio,
	}, message.MessageID)
	if err != nil {
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID,
			fmt.Sprintf(resources.Get("telegram.audio.send_failed"), err), message.MessageID, nil)
		return sendErr
	}
	return nil
}
