package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

var (
	errTranslationUnavailable = errors.New("перевод сейчас недоступен")
	errTranslationSource      = errors.New("не удалось получить текст публикации")
	errTranslationEmpty       = errors.New("нет текста для перевода")
)

type translationResult struct {
	Language    string `json:"language"`
	Translation string `json:"translation"`
	SourceURL   string `json:"source_url,omitempty"`
	Author      string `json:"author,omitempty"`
}

func (b *Bot) translationTool(message *telegram.Message) llm.Tool {
	return llm.Tool{
		Name:        "translate_text",
		Description: resources.Get("tool.translate_text.description"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language": map[string]any{
					"type": "string", "enum": []string{"ru", "en", "kk", "uk", "de", "fr", "es", "ja", "zh"},
					"default": "ru", "description": resources.Get("tool.translate_text.language_description"),
				},
				"text": map[string]any{
					"type": "string", "description": resources.Get("tool.translate_text.text_description"),
				},
				"url": map[string]any{
					"type": "string", "description": resources.Get("tool.translate_text.url_description"),
				},
			},
			"additionalProperties": false,
		},
		Execute: func(parent context.Context, arguments map[string]string) (string, error) {
			ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !b.twitterTranslationAllowed(message) {
				return "", errTranslationUnavailable
			}
			language := strings.ToLower(strings.TrimSpace(arguments["language"]))
			if language == "" {
				language = "ru"
			}
			if _, ok := translationLanguages[language]; !ok {
				return "", fmt.Errorf("неподдерживаемый язык %q; доступны ru, en, kk, uk, de, fr, es, ja, zh", language)
			}
			id, source, err := b.resolveTranslationSource(message, arguments["text"], arguments["url"])
			if err != nil {
				return "", err
			}
			result, err := b.translateResolvedSource(ctx, message, language, id, source)
			if err != nil {
				return "", err
			}
			encoded, err := json.Marshal(result)
			return string(encoded), err
		},
	}
}

// Resolve an explicit source or the user's reply without downloading attachments.
// Legacy keyboards remain readable so replies to older posts still get full text.
func (b *Bot) resolveTranslationSource(message *telegram.Message, text, rawURL string) (string, downloader.Post, error) {
	var source downloader.Post
	if strings.TrimSpace(text) != "" && strings.TrimSpace(rawURL) != "" {
		return "", source, errors.New("укажи только text или url, не оба сразу")
	}
	if strings.TrimSpace(rawURL) != "" {
		id := twitterPostID(rawURL)
		if id == "" {
			return "", source, errors.New("url должен быть ссылкой на публикацию X/Twitter; для другого источника передай его текст в text")
		}
		return id, source, nil
	}
	if strings.TrimSpace(text) != "" {
		source.Text = text
		return "", source, nil
	}
	if message == nil || message.ReplyToMessage == nil {
		return "", source, errors.New("передай text, ссылку X/Twitter в url или ответь на сообщение с текстом")
	}
	reply := message.ReplyToMessage
	id := twitterIDFromMarkup(reply.ReplyMarkup)
	if id == "" {
		id = twitterPostID(strings.TrimSpace(reply.ContentText()))
	}
	// Bot posts end with the canonical source URL; prefer it to quoted links.
	if id == "" && reply.From != nil && b.identity.ID != 0 && reply.From.ID == b.identity.ID {
		links := extractURLs(reply)
		for i := len(links) - 1; i >= 0 && id == ""; i-- {
			id = twitterPostID(links[i])
		}
	}
	if id != "" {
		return id, source, nil
	}
	source.Text = reply.ContentText()
	if strings.TrimSpace(source.Text) == "" {
		return "", source, errTranslationEmpty
	}
	return "", source, nil
}

// Both /translate and the LLM tool use the isolated translator. This method has
// no Telegram output and never changes the conversational model's history.
func (b *Bot) translateResolvedSource(ctx context.Context, message *telegram.Message, language, id string, source downloader.Post) (translationResult, error) {
	var result translationResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !b.twitterTranslationAllowed(message) {
		return result, errTranslationUnavailable
	}
	if id != "" {
		if b.downloader == nil {
			return result, errTranslationSource
		}
		var err error
		source, err = b.downloader.TwitterPost(ctx, "https://x.com/i/status/"+id)
		if err != nil {
			return result, fmt.Errorf("%w: %w", errTranslationSource, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !b.twitterTranslationAllowed(message) {
		return result, errTranslationUnavailable
	}
	if strings.TrimSpace(source.Text) == "" {
		return result, errTranslationEmpty
	}
	if b.state != nil {
		b.state.LLMCalls.Add(1)
	}
	translated, err := b.llm.Translate(ctx, source.Text, language)
	if err != nil {
		if b.state != nil {
			b.state.LLMErrors.Add(1)
		}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !b.twitterTranslationAllowed(message) {
		return result, errTranslationUnavailable
	}
	if strings.TrimSpace(translated) == "" {
		return result, errors.New("не удалось перевести текст")
	}
	return translationResult{Language: language, Translation: translated, SourceURL: source.URL, Author: source.Author}, nil
}

func translationErrorText(err error) string {
	switch {
	case errors.Is(err, errTranslationSource):
		return "Не удалось получить текст публикации. Попробуй позже."
	case errors.Is(err, errTranslationEmpty):
		return "В публикации нет текста для перевода."
	default:
		return "Не удалось перевести текст. Попробуй позже."
	}
}
