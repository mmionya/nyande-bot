package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/logutil"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) handleMediaURL(ctx context.Context, message *telegram.Message, mediaURL string) error {
	_, err := b.downloadAndSendMedia(ctx, message, mediaURL)
	return err
}

// Return confirmed deliveries so application tools can report failures and
// partial sends accurately instead of treating a displayed error as success.
func (b *Bot) downloadAndSendMedia(ctx context.Context, message *telegram.Message, mediaURL string) ([]telegram.Message, error) {
	started := time.Now()
	log.Printf("[media] download_started platform=telegram chat=%d user=%d msg=%d url=%q", message.Chat.ID, userID(message), message.MessageID, logutil.URL(mediaURL))
	b.state.MediaTotal.Add(1)
	status, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("downloader.status.downloading"), message.MessageID, nil)
	if err != nil {
		log.Printf("[media] status_failed platform=telegram chat=%d msg=%d elapsed_ms=%d", message.Chat.ID, message.MessageID, time.Since(started).Milliseconds())
		return nil, err
	}
	result, err := b.downloader.Download(ctx, mediaURL)
	if err == nil && len(result.Items) == 0 {
		err = errors.New("downloader returned no media")
	}
	if err != nil {
		b.state.MediaErrors.Add(1)
		log.Printf("[media] download_failed platform=telegram chat=%d msg=%d url=%q elapsed_ms=%d: %v", message.Chat.ID, message.MessageID, logutil.URL(mediaURL), time.Since(started).Milliseconds(), err)
		_ = b.telegram.EditMessageText(ctx, message.Chat.ID, status.MessageID,
			resources.Format("downloader.error.failed", map[string]any{"error": humanDownloadError(mediaURL, err)}), nil)
		return nil, err
	}
	log.Printf("[media] downloaded platform=telegram chat=%d msg=%d source=%q items=%d elapsed_ms=%d", message.Chat.ID, message.MessageID, result.Source, len(result.Items), time.Since(started).Milliseconds())
	for index, item := range result.Items {
		log.Printf("[media] file platform=telegram chat=%d msg=%d index=%d kind=%q name=%q bytes=%d", message.Chat.ID, message.MessageID, index+1, item.Kind, item.Name, len(item.Data))
	}
	switch result.Source {
	case "tiktok":
		b.state.TikTokDownloads.Add(1)
	case "instagram":
		b.state.InstagramDownloads.Add(1)
	case "twitter":
		b.state.TwitterDownloads.Add(1)
	case "reddit":
		b.state.RedditDownloads.Add(1)
	case "youtube":
		b.state.YouTubeDownloads.Add(1)
	case "xiaohongshu":
		b.state.XiaohongshuDownloads.Add(1)
	case "pinterest":
		b.state.PinterestDownloads.Add(1)
	}

	lock := b.state.ChatLock(message.Chat.ID)
	lock.Lock()
	defer lock.Unlock()
	sendStarted := time.Now()
	log.Printf("[media] send_started platform=telegram chat=%d msg=%d items=%d", message.Chat.ID, message.MessageID, len(result.Items))
	_ = b.telegram.EditMessageText(ctx, message.Chat.ID, status.MessageID, resources.Get("downloader.status.sending"), nil)
	sent, sendErr := b.sendDownloadedMedia(ctx, message, result)
	if len(sent) > 0 {
		attachments := make([]cachedAttachment, 0, len(sent))
		for _, item := range result.Items[:min(len(sent), len(result.Items))] {
			attachments = append(attachments, cachedAttachment{
				Kind: item.Kind, Name: item.Name, MIME: item.MIME, Data: item.Data,
			})
		}
		b.media.Put(message.Chat.ID, sent, attachments)
	}
	if sendErr != nil {
		b.state.MediaErrors.Add(1)
		log.Printf("[media] send_failed platform=telegram chat=%d msg=%d sent=%d expected=%d elapsed_ms=%d: %v", message.Chat.ID, message.MessageID, len(sent), len(result.Items), time.Since(sendStarted).Milliseconds(), sendErr)
		_ = b.telegram.EditMessageText(ctx, message.Chat.ID, status.MessageID,
			resources.Get("downloader.error.telegram_send"), nil)
		return sent, sendErr
	}
	_ = b.telegram.DeleteMessage(ctx, message.Chat.ID, status.MessageID)
	log.Printf("[media] sent platform=telegram chat=%d msg=%d items=%d send_ms=%d total_ms=%d", message.Chat.ID, message.MessageID, len(sent), time.Since(sendStarted).Milliseconds(), time.Since(started).Milliseconds())
	return sent, nil
}

func (b *Bot) sendDownloadedMedia(ctx context.Context, message *telegram.Message, result downloader.Result) ([]telegram.Message, error) {
	uploads := make([]telegram.Upload, 0, len(result.Items))
	for index, item := range result.Items {
		caption := ""
		if index == 0 {
			caption = result.Caption
		}
		uploads = append(uploads, telegram.Upload{
			Kind: item.Kind, Name: item.Name, MIME: item.MIME, Data: item.Data, URL: item.URL, Caption: caption,
		})
	}
	var sent []telegram.Message
	for start := 0; start < len(uploads); {
		end := min(start+10, len(uploads))
		chunk := uploads[start:end]
		canAlbum := len(chunk) >= 2
		for _, upload := range chunk {
			if upload.Kind == "animation" {
				canAlbum = false
				break
			}
		}
		if canAlbum {
			messages, err := b.telegram.SendMediaGroup(ctx, message.Chat.ID, chunk, message.MessageID)
			if err == nil {
				sent = append(sent, messages...)
				start = end
				continue
			}
			log.Printf("[media] album_fallback platform=telegram chat=%d msg=%d items=%d: %v", message.Chat.ID, message.MessageID, len(chunk), err)
		}
		upload := uploads[start]
		sentMessage, err := b.telegram.SendUpload(ctx, message.Chat.ID, upload, message.MessageID)
		if err != nil && upload.Kind != "document" {
			upload.Kind = "document"
			sentMessage, err = b.telegram.SendUpload(ctx, message.Chat.ID, upload, message.MessageID)
		}
		if err != nil {
			return sent, err
		}
		sent = append(sent, sentMessage)
		start++
	}
	return sent, nil
}

func humanDownloadError(rawURL string, err error) string {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "too large") || strings.Contains(text, "exceeds"):
		return resources.Get("downloader.error.too_large")
	case strings.Contains(text, "apify_token") || strings.Contains(text, "apify token"):
		return resources.Get("downloader.error.apify_missing")
	case strings.Contains(text, "404") || strings.Contains(text, "not found"):
		return resources.Get("downloader.error.not_found")
	case strings.Contains(text, "timeout") || strings.Contains(text, "deadline"):
		return resources.Get("downloader.error.timeout")
	case strings.Contains(text, "instagram download failed"):
		return resources.Get("downloader.error.instagram_failed")
	default:
		_ = rawURL
		return resources.Get("downloader.error.generic")
	}
}

func (b *Bot) handleLLM(ctx context.Context, message *telegram.Message) error {
	if !b.llm.Enabled() {
		return nil
	}
	text := strings.TrimSpace(message.ContentText())
	hasMedia := messageHasMedia(message)
	if text == "" && !hasMedia {
		return nil
	}
	if !message.IsPrivate() && !b.shouldHandleGroupLLM(message) {
		return nil
	}

	stopTyping := make(chan struct{})
	go b.typingLoop(ctx, message.Chat.ID, stopTyping)
	defer close(stopTyping)

	attachments, err := b.collectLLMAttachments(ctx, message)
	if err != nil {
		log.Printf("[llm] media collection failed: %v", err)
	}
	images := make([]llm.Image, 0)
	transcripts := make([]string, 0)
	for _, attachment := range attachments {
		switch attachment.Kind {
		case "photo":
			if b.cfg.LLMVision {
				images = append(images, llm.Image{MIME: attachment.MIME, Data: attachment.Data})
			}
		case "video", "animation", "video_note":
			if b.cfg.LLMVision {
				frames, frameErr := extractVideoFrames(ctx, attachment.Data, attachment.Name, b.cfg.LLMVideoFrames)
				if frameErr != nil {
					log.Printf("[llm] video frames failed: %v", frameErr)
				} else {
					images = append(images, frames...)
				}
			}
			if b.cfg.WhisperEnabled {
				transcript, transcriptErr := b.transcribe(ctx, attachment, true)
				if transcriptErr != nil {
					log.Printf("[llm] video transcription failed: %v", transcriptErr)
				} else if transcript != "" {
					transcripts = append(transcripts, transcript)
				}
			}
		case "audio", "voice":
			if b.cfg.WhisperEnabled {
				transcript, transcriptErr := b.transcribe(ctx, attachment, false)
				if transcriptErr != nil {
					log.Printf("[llm] audio transcription failed: %v", transcriptErr)
				} else if transcript != "" {
					transcripts = append(transcripts, transcript)
				}
			}
		}
	}
	if len(images) > 12 {
		images = images[:12]
	}
	if text == "" && len(images) == 0 && len(transcripts) == 0 {
		if err != nil && isFileTooBig(err) {
			_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID,
				"Файл слишком большой! Telegram Bot API не разрешает ботам скачивать файлы размером более 20 МБ (ㅠ﹏ㅠ). Отправь файл размером до 20 МБ :3",
				message.MessageID, nil)
			return sendErr
		}
		return nil
	}
	if err != nil && isFileTooBig(err) {
		text += "\n\n[Системное уведомление: прикреплённый пользователем медиафайл весит больше 20 МБ, поэтому Telegram Bot API не разрешил боту его скачать. Объясни пользователю, что файл превышает лимит Telegram для ботов в 20 МБ]"
	}
	var mediaSpecs []string
	for _, att := range attachments {
		if spec, err := inspectMedia(ctx, att); err == nil && spec != "" {
			mediaSpecs = append(mediaSpecs, spec)
		}
	}
	if len(mediaSpecs) > 0 {
		text += "\n\n[Технические характеристики прикреплённого медиа:\n" + strings.Join(mediaSpecs, "\n\n") + "]"
	}

	b.state.LLMCalls.Add(1)
	userName := ""
	if message.From != nil {
		userName = message.From.FirstName
	}
	tools := []llm.Tool{b.tomatoTool(message), b.downloadMediaTool(message)}
	tools = append(tools, b.llm.MediaSearchTools()...)
	tools = append(tools, b.memoryTools(message)...)
	tools = append(tools, b.chatlogTools(message)...)
	tools = append(tools, b.reminderTools(message)...)
	answer, err := b.llm.Ask(ctx, llm.Request{
		ChatID: message.Chat.ID, UserName: userName, Text: text,
		Images: images, Transcripts: transcripts, Memories: b.recallMemories(ctx, message, text), Tools: tools,
	})
	if err != nil {
		b.state.LLMErrors.Add(1)
		log.Printf("[llm] chat=%d failed: %v", message.Chat.ID, err)
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID,
			resources.Format("llm.error.request_failed", map[string]any{"error": err}), message.MessageID, nil)
		return sendErr
	}
	return b.sendLLMAnswer(ctx, message, answer)
}

func (b *Bot) shouldHandleGroupLLM(message *telegram.Message) bool {
	reply := message.ReplyToMessage
	triggered := containsTriggerWord(message.ContentText(), b.cfg.LLMTriggerWords)
	if reply != nil && b.media != nil && b.media.Has(message.Chat.ID, reply.MessageID) {
		return triggered
	}
	if triggered {
		return true
	}
	addressed := b.isMentioned(message) || (reply != nil && reply.From != nil && reply.From.ID == b.identity.ID)
	return addressed
}

func containsTriggerWord(text string, triggers []string) bool {
	text = strings.ToLower(text)
	for _, trigger := range triggers {
		trigger = strings.ToLower(strings.TrimSpace(trigger))
		if trigger == "" {
			continue
		}
		for offset := 0; offset < len(text); {
			index := strings.Index(text[offset:], trigger)
			if index < 0 {
				break
			}
			start := offset + index
			end := start + len(trigger)
			if triggerBoundaryBefore(text, start) && triggerBoundaryAfter(text, end) {
				return true
			}
			offset = start + 1
		}
	}
	return false
}

func triggerBoundaryBefore(text string, index int) bool {
	if index == 0 {
		return true
	}
	character, _ := utf8.DecodeLastRuneInString(text[:index])
	return !isTriggerWordCharacter(character)
}

func triggerBoundaryAfter(text string, index int) bool {
	if index == len(text) {
		return true
	}
	character, _ := utf8.DecodeRuneInString(text[index:])
	return !isTriggerWordCharacter(character)
}

func isTriggerWordCharacter(character rune) bool {
	return unicode.IsLetter(character) || unicode.IsNumber(character) || character == '_'
}

func (b *Bot) collectLLMAttachments(ctx context.Context, message *telegram.Message) ([]cachedAttachment, error) {
	if !messageHasMedia(message) && message.ReplyToMessage != nil {
		if cached := b.media.Get(message.Chat.ID, message.ReplyToMessage.MessageID); len(cached) > 0 {
			return cached, nil
		}
		message = message.ReplyToMessage
	}
	if !messageHasMedia(message) {
		return nil, nil
	}
	var descriptors []struct {
		kind     string
		name     string
		mime     string
		fileID   string
		fileSize int64
	}
	if len(message.Photo) > 0 {
		photo := message.Photo[len(message.Photo)-1]
		descriptors = append(descriptors, struct {
			kind     string
			name     string
			mime     string
			fileID   string
			fileSize int64
		}{"photo", "photo.jpg", "image/jpeg", photo.FileID, photo.FileSize})
	}
	if message.Video != nil {
		descriptors = append(descriptors, struct {
			kind     string
			name     string
			mime     string
			fileID   string
			fileSize int64
		}{"video", firstString(message.Video.FileName, "video.mp4"), firstString(message.Video.MimeType, "video/mp4"), message.Video.FileID, message.Video.FileSize})
	}
	if message.Animation != nil {
		descriptors = append(descriptors, struct {
			kind     string
			name     string
			mime     string
			fileID   string
			fileSize int64
		}{"animation", firstString(message.Animation.FileName, "animation.mp4"), firstString(message.Animation.MimeType, "video/mp4"), message.Animation.FileID, message.Animation.FileSize})
	}
	if message.Voice != nil {
		descriptors = append(descriptors, struct {
			kind     string
			name     string
			mime     string
			fileID   string
			fileSize int64
		}{"voice", "voice.ogg", firstString(message.Voice.MimeType, "audio/ogg"), message.Voice.FileID, message.Voice.FileSize})
	}
	if message.Audio != nil {
		descriptors = append(descriptors, struct {
			kind     string
			name     string
			mime     string
			fileID   string
			fileSize int64
		}{"audio", firstString(message.Audio.FileName, "audio.mp3"), firstString(message.Audio.MimeType, "audio/mpeg"), message.Audio.FileID, message.Audio.FileSize})
	}
	if message.VideoNote != nil {
		descriptors = append(descriptors, struct {
			kind     string
			name     string
			mime     string
			fileID   string
			fileSize int64
		}{"video_note", "video_note.mp4", "video/mp4", message.VideoNote.FileID, message.VideoNote.FileSize})
	}
	if message.Document != nil {
		mime := strings.ToLower(firstString(message.Document.MimeType, "application/octet-stream"))
		kind := ""
		switch {
		case strings.HasPrefix(mime, "image/"):
			kind = "photo"
		case strings.HasPrefix(mime, "video/"):
			kind = "video"
		case strings.HasPrefix(mime, "audio/"):
			kind = "audio"
		}
		if kind != "" {
			descriptors = append(descriptors, struct {
				kind     string
				name     string
				mime     string
				fileID   string
				fileSize int64
			}{kind, firstString(message.Document.FileName, "document.bin"), mime, message.Document.FileID, message.Document.FileSize})
		}
	}
	attachments := make([]cachedAttachment, 0, len(descriptors))
	var combined error
	for _, descriptor := range descriptors {
		if descriptor.fileSize > 20*1024*1024 {
			combined = errors.Join(combined, fmt.Errorf("файл %s (%s) превышает лимит Telegram для скачивания ботами (максимум 20 МБ)", descriptor.name, formatFileSize(descriptor.fileSize)))
			continue
		}
		data, filePath, err := b.telegram.DownloadFile(ctx, descriptor.fileID, b.cfg.MaxFileSize)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "file is too big") {
				err = fmt.Errorf("файл %s превышает лимит Telegram для скачивания ботами (максимум 20 МБ): %w", descriptor.name, err)
			}
			combined = errors.Join(combined, err)
			continue
		}
		name := descriptor.name
		if name == "" && filePath != "" {
			name = filepath.Base(filePath)
		}
		attachments = append(attachments, cachedAttachment{
			Kind: descriptor.kind, Name: name, MIME: descriptor.mime, Data: data,
		})
	}
	return attachments, combined
}

func messageHasMedia(message *telegram.Message) bool {
	if message == nil {
		return false
	}
	if len(message.Photo) > 0 || message.Video != nil || message.Animation != nil ||
		message.Audio != nil || message.Voice != nil || message.VideoNote != nil {
		return true
	}
	if message.Document != nil {
		mime := strings.ToLower(message.Document.MimeType)
		return strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/")
	}
	return false
}

func (b *Bot) isMentioned(message *telegram.Message) bool {
	expected := "@" + strings.ToLower(b.identity.Username)
	text := message.ContentText()
	for _, entity := range message.ContentEntities() {
		if entity.Type == "mention" && strings.ToLower(sliceUTF16(text, entity.Offset, entity.Length)) == expected {
			return true
		}
	}
	return false
}

func (b *Bot) typingLoop(ctx context.Context, chatID int64, stop <-chan struct{}) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	_ = b.telegram.SendChatAction(ctx, chatID, "typing")
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			_ = b.telegram.SendChatAction(ctx, chatID, "typing")
		}
	}
}

func extractVideoFrames(ctx context.Context, data []byte, name string, count int) ([]llm.Image, error) {
	directory, err := os.MkdirTemp("", "nyande-frames-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "input"+safeExtension(name, ".mp4"))
	if err := os.WriteFile(input, data, 0o600); err != nil {
		return nil, err
	}
	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", input)
	probeOutput, err := probe.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w: %s", err, strings.TrimSpace(string(probeOutput)))
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(probeOutput)), 64)
	if err != nil || duration <= 0 {
		return nil, errors.New("invalid video duration")
	}
	frames := make([]llm.Image, 0, count)
	for index := 0; index < count; index++ {
		position := duration * float64(index+1) / float64(count+1)
		output := filepath.Join(directory, fmt.Sprintf("frame-%02d.jpg", index))
		command := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
			"-ss", fmt.Sprintf("%.3f", position), "-i", input, "-frames:v", "1",
			"-vf", `scale=min(1280\,iw):-2`, "-q:v", "3", "-y", output)
		commandOutput, commandErr := command.CombinedOutput()
		if commandErr != nil {
			log.Printf("[llm] frame %d failed: %v: %s", index, commandErr, strings.TrimSpace(string(commandOutput)))
			continue
		}
		frame, readErr := os.ReadFile(output)
		if readErr == nil {
			frames = append(frames, llm.Image{MIME: "image/jpeg", Data: frame})
		}
	}
	if len(frames) == 0 {
		return nil, errors.New("ffmpeg produced no video frames")
	}
	return frames, nil
}

func (b *Bot) transcribe(ctx context.Context, attachment cachedAttachment, video bool) (string, error) {
	directory, err := os.MkdirTemp("", "nyande-whisper-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(directory)
	input := filepath.Join(directory, "input"+safeExtension(attachment.Name, extensionForMIME(attachment.MIME)))
	if err := os.WriteFile(input, attachment.Data, 0o600); err != nil {
		return "", err
	}
	if video {
		audio := filepath.Join(directory, "audio.wav")
		command := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
			"-i", input, "-vn", "-ac", "1", "-ar", "16000", "-y", audio)
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ffmpeg audio extraction failed: %w: %s", err, strings.TrimSpace(string(output)))
		}
		input = audio
	}
	arguments := []string{input, "--model", b.cfg.WhisperModel, "--output_dir", directory, "--output_format", "txt"}
	if language := strings.TrimSpace(b.cfg.WhisperLanguage); language != "" && !strings.EqualFold(language, "auto") {
		arguments = append(arguments, "--language", language)
	}
	command := exec.CommandContext(ctx, b.cfg.WhisperPath, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Whisper failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	textFiles, _ := filepath.Glob(filepath.Join(directory, "*.txt"))
	if len(textFiles) == 0 {
		return "", errors.New("Whisper produced no transcript")
	}
	transcript, err := os.ReadFile(textFiles[0])
	return strings.TrimSpace(string(transcript)), err
}

func safeExtension(name, fallback string) string {
	extension := strings.ToLower(filepath.Ext(name))
	if len(extension) < 2 || len(extension) > 8 {
		return fallback
	}
	for _, character := range extension[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return fallback
		}
	}
	return extension
}

func extensionForMIME(mimeType string) string {
	extensions, _ := mime.ExtensionsByType(strings.Split(mimeType, ";")[0])
	if len(extensions) > 0 {
		return extensions[0]
	}
	switch {
	case strings.HasPrefix(mimeType, "audio/"):
		return ".ogg"
	case strings.HasPrefix(mimeType, "video/"):
		return ".mp4"
	default:
		return ".bin"
	}
}

func firstString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
