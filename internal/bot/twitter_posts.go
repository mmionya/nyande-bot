package bot

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

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

const translationUsage = "Используй /translate [ru|en|kk|uk|de|fr|es|ja|zh] [текст или ссылка на X/Twitter] или ответь этой командой на текст. По умолчанию перевожу на русский."

func twitterPostID(rawURL string) string {
	parsed, err := url.Parse(normalizeLink(rawURL))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.Port() != "" || !downloader.IsTwitter(parsed) {
		return ""
	}
	match := twitterTranslationPath.FindStringSubmatch(parsed.Path)
	if len(match) != 2 {
		return ""
	}
	return match[1]
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
		part, err := b.telegram.SendMessage(ctx, message.Chat.ID, chunk, message.MessageID, nil)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if message == nil || message.From == nil || message.From.IsBot || message.SenderChat != nil || b.blockedMessage(message) || b.silentModeration(message.Chat.ID) {
		return nil
	}
	if !b.twitterTranslationAllowed(message) {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, "Перевод сейчас недоступен: языковая модель отключена.", message.MessageID, nil)
		return err
	}
	language := "ru"
	text := strings.TrimSpace(arguments)
	first, rest := text, ""
	if end := strings.IndexFunc(text, unicode.IsSpace); end >= 0 {
		first, rest = text[:end], strings.TrimLeftFunc(text[end:], unicode.IsSpace)
	}
	if _, ok := translationLanguages[strings.ToLower(first)]; ok {
		language, text = strings.ToLower(first), rest
	}
	var rawURL string
	if !strings.ContainsFunc(text, unicode.IsSpace) && (twitterPostID(text) != "" || strings.Contains(text, "://") || strings.HasPrefix(text, "x.com/") || strings.HasPrefix(text, "twitter.com/")) {
		rawURL, text = text, ""
	}
	id, source, err := b.resolveTranslationSource(message, text, rawURL)
	if err != nil {
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
	result, err := b.translateResolvedSource(ctx, message, language, id, source)
	if err != nil {
		if errors.Is(err, errTranslationUnavailable) {
			return nil
		}
		return fail(err, translationErrorText(err))
	}
	text := "Перевод на " + translationLanguages[language] + ":\n\n" + result.Translation + twitterPostFooter(downloader.Post{Author: result.Author, URL: result.SourceURL})
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
