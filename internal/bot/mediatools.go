package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/internal/telegram"
	_ "golang.org/x/image/webp"
)

type probeOutput struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		Size     string `json:"size"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

func inspectMedia(ctx context.Context, att cachedAttachment) (string, error) {
	sizeStr := formatFileSize(int64(len(att.Data)))

	if att.Kind == "photo" {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(att.Data))
		if err == nil && cfg.Width > 0 && cfg.Height > 0 {
			aspect := formatAspect(cfg.Width, cfg.Height)
			return fmt.Sprintf("🖼 **Фотография**\n• Разрешение: `%dx%d px` (%s)\n• Формат: `%s`\n• Размер файла: `%s`",
				cfg.Width, cfg.Height, aspect, strings.ToUpper(format), sizeStr), nil
		}
		return fmt.Sprintf("🖼 **Фотография**\n• Формат: `%s`\n• Размер файла: `%s`", att.MIME, sizeStr), nil
	}

	// For video/animation/audio, use ffprobe
	tempDir, err := os.MkdirTemp("", "nyande-inspect-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tempDir)

	input := filepath.Join(tempDir, "input"+safeExtension(att.Name, extensionForMIME(att.MIME)))
	if err := os.WriteFile(input, att.Data, 0o600); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("📁 **Медиафайл**\n• Имя: `%s`\n• MIME: `%s`\n• Размер: `%s`", att.Name, att.MIME, sizeStr), nil
	}

	var probed probeOutput
	if err := json.Unmarshal(output, &probed); err != nil {
		return fmt.Sprintf("📁 **Медиафайл**\n• MIME: `%s`\n• Размер: `%s`", att.MIME, sizeStr), nil
	}

	var vStream, aStream *struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	}

	for i := range probed.Streams {
		s := &probed.Streams[i]
		if s.CodecType == "video" && vStream == nil {
			vStream = s
		} else if s.CodecType == "audio" && aStream == nil {
			aStream = s
		}
	}

	durationSec := parseDuration(probed.Format.Duration)
	if durationSec <= 0 && vStream != nil {
		durationSec = parseDuration(vStream.Duration)
	}

	durStr := formatDuration(durationSec)
	totalBitrate := parseBitrate(probed.Format.BitRate)

	if vStream != nil {
		fps := parseFPS(vStream.RFrameRate)
		aspect := formatAspect(vStream.Width, vStream.Height)
		var bld strings.Builder
		header := "🎬 **Видео**"
		if att.Kind == "animation" {
			header = "🎞 **Анимация (GIF)**"
		}
		fmt.Fprintf(&bld, "%s\n", header)
		fmt.Fprintf(&bld, "• Разрешение: `%dx%d px` (%s)\n", vStream.Width, vStream.Height, aspect)
		if fps > 0 {
			fmt.Fprintf(&bld, "• Частота кадров: `%.2f fps`\n", fps)
		}
		if durationSec > 0 {
			fmt.Fprintf(&bld, "• Длительность: `%s` (%.1f с)\n", durStr, durationSec)
		}
		fmt.Fprintf(&bld, "• Видеокодек: `%s`\n", vStream.CodecName)
		if aStream != nil {
			chStr := "моно"
			if aStream.Channels == 2 {
				chStr = "стерео"
			} else if aStream.Channels > 2 {
				chStr = fmt.Sprintf("%d кан.", aStream.Channels)
			}
			fmt.Fprintf(&bld, "• Аудиокодек: `%s` (%s Hz, %s)\n", aStream.CodecName, aStream.SampleRate, chStr)
		}
		if totalBitrate > 0 {
			fmt.Fprintf(&bld, "• Общий битрейт: `%d kbps`\n", totalBitrate/1000)
		}
		fmt.Fprintf(&bld, "• Размер файла: `%s`", sizeStr)
		return bld.String(), nil
	}

	if aStream != nil {
		header := "🎵 **Аудиозапись**"
		if att.Kind == "voice" {
			header = "🎙 **Голосовое сообщение**"
		}
		var bld strings.Builder
		fmt.Fprintf(&bld, "%s\n", header)
		if durationSec > 0 {
			fmt.Fprintf(&bld, "• Длительность: `%s` (%.1f с)\n", durStr, durationSec)
		}
		chStr := "моно"
		if aStream.Channels == 2 {
			chStr = "стерео"
		}
		fmt.Fprintf(&bld, "• Аудиокодек: `%s` (%s Hz, %s)\n", aStream.CodecName, aStream.SampleRate, chStr)
		if totalBitrate > 0 {
			fmt.Fprintf(&bld, "• Битрейт: `%d kbps`\n", totalBitrate/1000)
		}
		fmt.Fprintf(&bld, "• Размер файла: `%s`", sizeStr)
		return bld.String(), nil
	}

	return fmt.Sprintf("📁 **Медиафайл**\n• Размер: `%s`", sizeStr), nil
}

func convertVideoToRound(ctx context.Context, data []byte, name string) ([]byte, error) {
	tempDir, err := os.MkdirTemp("", "nyande-round-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	input := filepath.Join(tempDir, "input"+safeExtension(name, ".mp4"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}

	output := filepath.Join(tempDir, "round.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", input,
		"-vf", "crop='min(iw,ih)':'min(iw,ih)',scale=384:384,fps=30",
		"-c:v", "libx264", "-preset", "fast", "-crf", "23",
		"-c:a", "aac", "-b:a", "64k",
		"-t", "60",
		"-movflags", "+faststart",
		"-y", output,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg round conversion failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(output)
}

func convertAudioToVoice(ctx context.Context, data []byte, name string) ([]byte, error) {
	tempDir, err := os.MkdirTemp("", "nyande-voice-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	input := filepath.Join(tempDir, "input"+safeExtension(name, ".mp3"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}

	output := filepath.Join(tempDir, "voice.ogg")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", input,
		"-vn",
		"-c:a", "libopus", "-b:a", "64k", "-ar", "48000",
		"-y", output,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg voice conversion failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(output)
}

func convertVideoToGIF(ctx context.Context, data []byte, name string) ([]byte, error) {
	tempDir, err := os.MkdirTemp("", "nyande-gif-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	input := filepath.Join(tempDir, "input"+safeExtension(name, ".mp4"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}

	output := filepath.Join(tempDir, "animation.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", input,
		"-an",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-y", output,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg animation conversion failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(output)
}

func (b *Bot) roundCommand(ctx context.Context, message *telegram.Message) error {
	attachments, err := b.collectLLMAttachments(ctx, message)
	if err != nil {
		logError("round attachments", err)
	}
	var target *cachedAttachment
	for i := range attachments {
		if attachments[i].Kind == "video" || attachments[i].Kind == "animation" {
			target = &attachments[i]
			break
		}
	}
	if target == nil {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID,
			"Пожалуйста, прикрепи видео к команде /round или ответь этой командой на видео соощение :3",
			message.MessageID, nil)
		return err
	}

	roundData, err := convertVideoToRound(ctx, target.Data, target.Name)
	if err != nil {
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID,
			fmt.Sprintf("Не удалось конвертировать видео в кружочек: %v", err),
			message.MessageID, nil)
		return sendErr
	}

	_, err = b.telegram.SendVideoNote(ctx, message.Chat.ID, roundData, message.MessageID)
	return err
}

func (b *Bot) voiceCommand(ctx context.Context, message *telegram.Message) error {
	attachments, err := b.collectLLMAttachments(ctx, message)
	if err != nil {
		logError("voice attachments", err)
	}
	var target *cachedAttachment
	for i := range attachments {
		if attachments[i].Kind == "audio" || attachments[i].Kind == "voice" || attachments[i].Kind == "video" || attachments[i].Kind == "animation" {
			target = &attachments[i]
			break
		}
	}
	if target == nil {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID,
			"Пожалуйста, прикрепи аудио или видео к команде /voice или ответь этой командой на медиа :3",
			message.MessageID, nil)
		return err
	}

	voiceData, err := convertAudioToVoice(ctx, target.Data, target.Name)
	if err != nil {
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID,
			fmt.Sprintf("Не удалось конвертировать в голосовое сообщение: %v", err),
			message.MessageID, nil)
		return sendErr
	}

	_, err = b.telegram.SendVoice(ctx, message.Chat.ID, voiceData, "", message.MessageID)
	return err
}

func (b *Bot) gifCommand(ctx context.Context, message *telegram.Message) error {
	attachments, err := b.collectLLMAttachments(ctx, message)
	if err != nil {
		logError("gif attachments", err)
	}
	var target *cachedAttachment
	for i := range attachments {
		if attachments[i].Kind == "video" || attachments[i].Kind == "animation" {
			target = &attachments[i]
			break
		}
	}
	if target == nil {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID,
			"Пожалуйста, прикрепи видео к команде /gif или ответь этой командой на видео :3",
			message.MessageID, nil)
		return err
	}

	gifData, err := convertVideoToGIF(ctx, target.Data, target.Name)
	if err != nil {
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID,
			fmt.Sprintf("Не удалось конвертировать видео в гифку: %v", err),
			message.MessageID, nil)
		return sendErr
	}

	_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
		Kind: "animation",
		Name: "animation.mp4",
		MIME: "video/mp4",
		Data: gifData,
	}, message.MessageID)
	return err
}

func (b *Bot) mediainfoCommand(ctx context.Context, message *telegram.Message) error {
	attachments, err := b.collectLLMAttachments(ctx, message)
	if err != nil {
		logError("mediainfo attachments", err)
	}
	if len(attachments) == 0 {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID,
			"Пожалуйста, прикрепи фото, видео или аудио к команде /mediainfo или ответь этой командой на медиа :3",
			message.MessageID, nil)
		return err
	}

	var results []string
	for _, att := range attachments {
		info, err := inspectMedia(ctx, att)
		if err != nil {
			info = fmt.Sprintf("Ошибка анализа %s: %v", att.Name, err)
		}
		results = append(results, info)
	}

	text := strings.Join(results, "\n\n")
	_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, nil)
	return sendErr
}

func parseFPS(rFrameRate string) float64 {
	parts := strings.Split(rFrameRate, "/")
	if len(parts) == 1 {
		val, _ := strconv.ParseFloat(parts[0], 64)
		return val
	}
	if len(parts) == 2 {
		num, err1 := strconv.ParseFloat(parts[0], 64)
		den, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 == nil && err2 == nil && den > 0 {
			return num / den
		}
	}
	return 0
}

func parseDuration(s string) float64 {
	val, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return val
}

func parseBitrate(s string) int64 {
	val, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return val
}

func formatDuration(sec float64) string {
	if sec <= 0 {
		return "00:00"
	}
	totalSec := int(sec)
	m := totalSec / 60
	s := totalSec % 60
	if m >= 60 {
		h := m / 60
		m = m % 60
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func formatFileSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d Б", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f КБ", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.2f МБ", float64(bytes)/(1024*1024))
}

func formatAspect(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	gcd := func(a, b int) int {
		for b != 0 {
			a, b = b, a%b
		}
		return a
	}(w, h)
	aw, ah := w/gcd, h/gcd
	if (aw == 16 && ah == 9) || (aw == 4 && ah == 3) || (aw == 1 && ah == 1) || (aw == 9 && ah == 16) || (aw == 4 && ah == 5) {
		return fmt.Sprintf("%d:%d", aw, ah)
	}
	ratio := float64(w) / float64(h)
	return fmt.Sprintf("%.2f:1", ratio)
}

func logError(context string, err error) {
	if err != nil {
		fmt.Printf("[%s] error: %v\n", context, err)
	}
}
