package bot

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

const twitterTranslatePrefix = "twtranslate:"

var (
	twitterTranslationID   = regexp.MustCompile(`^[0-9]{1,25}$`)
	twitterTranslationPath = regexp.MustCompile(`^/(?:[A-Za-z0-9_]+/status|i/web/status)/([0-9]{1,25})(?:/[^?#]*)?$`)
	translationLanguages   = map[string]string{
		"ru": "русский", "en": "английский", "kk": "казахский", "uk": "украинский",
		"de": "немецкий", "fr": "французский", "es": "испанский", "ja": "японский", "zh": "китайский",
	}
)

const translationUsage = "Используй /translate [ru|en|kk|uk|de|fr|es|ja|zh] [ссылка на X/Twitter] или ответь этой командой на текст. По умолчанию перевожу на русский."

func twitterPostID(rawURL string) string {
	parsed, err := url.Parse(normalizeLink(rawURL))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || !downloader.IsTwitter(parsed) {
		return ""
	}
	match := twitterTranslationPath.FindStringSubmatch(parsed.Path)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func (b *Bot) twitterTranslationMarkup(post downloader.Post) any {
	if b.llm == nil || !b.llm.Enabled() {
		return nil
	}
	id := twitterPostID(post.URL)
	if id == "" || strings.TrimSpace(post.Text) == "" {
		return nil
	}
	return map[string]any{"inline_keyboard": [][]map[string]string{{{
		"text": "Перевести на русский", "callback_data": twitterTranslatePrefix + id,
	}}}}
}

// Preserve source whitespace and literal Markdown, including at message boundaries.
func splitTwitterPlainText(text string, limit int) []string {
	if text == "" || limit < 2 {
		return nil
	}
	var parts []string
	start, units := 0, 0
	for offset, r := range text {
		size := 1
		if r > 0xffff {
			size = 2
		}
		if units+size > limit {
			parts = append(parts, text[start:offset])
			start, units = offset, 0
		}
		units += size
	}
	return append(parts, text[start:])
}

func twitterPostFooter(post downloader.Post) string {
	var lines []string
	if author := strings.TrimSpace(post.Author); author != "" {
		lines = append(lines, author)
	}
	if twitterPostID(post.URL) != "" {
		lines = append(lines, post.URL)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(lines, "\n")
}

func (b *Bot) sendTwitterTextPost(ctx context.Context, message *telegram.Message, post downloader.Post) ([]telegram.Message, error) {
	if strings.TrimSpace(post.Text) == "" {
		return nil, errors.New("в публикации нет текста")
	}
	var sent []telegram.Message
	for _, chunk := range splitTwitterPlainText(post.Text+twitterPostFooter(post), telegramMessageLimit) {
		if err := ctx.Err(); err != nil {
			return sent, err
		}
		var markup any
		if !b.llmDisabled(message.Chat.ID) {
			markup = b.twitterTranslationMarkup(post)
		}
		part, err := b.telegram.SendMessage(ctx, message.Chat.ID, chunk, message.MessageID, markup)
		if err != nil {
			return sent, err
		}
		sent = append(sent, part)
	}
	return sent, nil
}

func (b *Bot) twitterTranslationAllowed(message *telegram.Message) bool {
	return message != nil && message.From != nil && !message.From.IsBot && message.SenderChat == nil &&
		!b.blockedMessage(message) && !b.silentModeration(message.Chat.ID) && !b.llmDisabled(message.Chat.ID) &&
		b.llm != nil && b.llm.Enabled()
}

func twitterIDFromMarkup(markup *telegram.InlineKeyboardMarkup) string {
	if markup == nil {
		return ""
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if id, ok := strings.CutPrefix(button.CallbackData, twitterTranslatePrefix); ok && twitterTranslationID.MatchString(id) {
				return id
			}
		}
	}
	return ""
}

func (b *Bot) translateCommand(ctx context.Context, message *telegram.Message, arguments string) error {
	if message == nil || message.From == nil || message.From.IsBot || message.SenderChat != nil || b.blockedMessage(message) || b.silentModeration(message.Chat.ID) {
		return nil
	}
	if !b.twitterTranslationAllowed(message) {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, "Перевод сейчас недоступен: языковая модель отключена.", message.MessageID, nil)
		return err
	}
	language := "ru"
	fields := strings.Fields(arguments)
	if len(fields) > 0 {
		if _, ok := translationLanguages[strings.ToLower(fields[0])]; ok {
			language, fields = strings.ToLower(fields[0]), fields[1:]
		}
	}
	var source downloader.Post
	var id string
	if len(fields) == 1 {
		id = twitterPostID(fields[0])
	} else if len(fields) == 0 && message.ReplyToMessage != nil {
		reply := message.ReplyToMessage
		id = twitterIDFromMarkup(reply.ReplyMarkup)
		if id == "" {
			id = twitterPostID(strings.TrimSpace(reply.ContentText()))
		}
		// Bot posts end with the canonical source URL; prefer it to links quoted in the post.
		if id == "" && reply.From != nil && b.identity.ID != 0 && reply.From.ID == b.identity.ID {
			links := extractURLs(reply)
			for i := len(links) - 1; i >= 0 && id == ""; i-- {
				id = twitterPostID(links[i])
			}
		}
		if id == "" {
			source.Text = reply.ContentText()
		}
	}
	if len(fields) > 1 || (id == "" && strings.TrimSpace(source.Text) == "") {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, translationUsage, message.MessageID, nil)
		return err
	}
	return b.translateTwitterSource(ctx, message, language, id, source)
}

func (b *Bot) handleTwitterTranslationCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	if callback == nil {
		return nil
	}
	answer := func(text string, alert bool) error {
		ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return b.telegram.AnswerCallback(ackCtx, callback.ID, text, "", alert)
	}
	if callback.Message == nil {
		return answer("Не удалось определить публикацию. Используй /translate со ссылкой.", true)
	}
	message := &telegram.Message{Chat: callback.Message.Chat, MessageID: callback.Message.MessageID, From: &callback.From}
	if callback.From.ID == 0 || callback.From.IsBot || b.blockedMessage(message) || b.silentModeration(message.Chat.ID) {
		return answer("", false)
	}
	id, valid := strings.CutPrefix(callback.Data, twitterTranslatePrefix)
	if !valid || !twitterTranslationID.MatchString(id) {
		return answer("Некорректная кнопка перевода.", true)
	}
	if !b.twitterTranslationAllowed(message) {
		return answer("Перевод сейчас недоступен: языковая модель отключена.", true)
	}
	_ = answer("Перевожу на русский…", false)
	return b.translateTwitterSource(ctx, message, "ru", id, downloader.Post{})
}

func (b *Bot) translateTwitterSource(parent context.Context, message *telegram.Message, language, id string, source downloader.Post) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !b.twitterTranslationAllowed(message) {
		return nil
	}
	status, err := b.telegram.SendMessage(ctx, message.Chat.ID, "Перевожу…", message.MessageID, nil)
	if err != nil {
		return err
	}
	fail := func(err error, text string) error {
		if parent.Err() == nil && b.twitterTranslationAllowed(message) {
			if errors.Is(err, context.DeadlineExceeded) {
				text = "Не удалось завершить перевод вовремя. Попробуй позже."
			}
			finishCtx, finishCancel := context.WithTimeout(parent, 5*time.Second)
			defer finishCancel()
			_ = b.telegram.EditMessageText(finishCtx, message.Chat.ID, status.MessageID, text, nil)
		}
		return err
	}
	if id != "" {
		if !b.twitterTranslationAllowed(message) {
			return nil
		}
		if b.downloader == nil {
			return fail(errors.New("twitter reader unavailable"), "Не удалось получить текст публикации. Попробуй позже.")
		}
		source, err = b.downloader.TwitterPost(ctx, "https://x.com/i/status/"+id)
		if err != nil {
			return fail(err, "Не удалось получить текст публикации. Попробуй позже.")
		}
	}
	if strings.TrimSpace(source.Text) == "" {
		return fail(errors.New("empty translation source"), "В публикации нет текста для перевода.")
	}
	if !b.twitterTranslationAllowed(message) {
		return nil
	}
	if b.state != nil {
		b.state.LLMCalls.Add(1)
	}
	translated, err := b.llm.Translate(ctx, source.Text, language)
	if err != nil {
		if b.state != nil {
			b.state.LLMErrors.Add(1)
		}
		return fail(err, "Не удалось перевести текст. Попробуй позже.")
	}
	if strings.TrimSpace(translated) == "" {
		return fail(errors.New("empty translation"), "Не удалось перевести текст. Попробуй позже.")
	}
	text := "Перевод на " + translationLanguages[language] + ":\n\n" + translated + twitterPostFooter(source)
	for _, chunk := range splitTwitterPlainText(text, telegramMessageLimit) {
		if err := ctx.Err(); err != nil {
			return fail(err, "Не удалось отправить перевод полностью.")
		}
		if !b.twitterTranslationAllowed(message) {
			return nil
		}
		if _, err := b.telegram.SendMessage(ctx, message.Chat.ID, chunk, message.MessageID, nil); err != nil {
			return fail(err, "Не удалось отправить перевод полностью.")
		}
	}
	if ctx.Err() == nil && b.twitterTranslationAllowed(message) {
		_ = b.telegram.DeleteMessage(ctx, message.Chat.ID, status.MessageID)
	}
	return nil
}
