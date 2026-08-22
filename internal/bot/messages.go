package bot

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/hyphentae/nyande-bot/internal/telegram"
)

const telegramMessageLimit = 4096

func (b *Bot) sendLLMAnswer(ctx context.Context, message *telegram.Message, answer string) error {
	for index, chunk := range splitTelegramMessage(answer, telegramMessageLimit) {
		replyTo := 0
		if index == 0 {
			replyTo = message.MessageID
		}
		if _, err := b.telegram.SendMessage(ctx, message.Chat.ID, chunk, replyTo, nil); err != nil {
			return err
		}
	}
	return nil
}

func splitTelegramMessage(value string, limit int) []string {
	value = strings.TrimSpace(value)
	if value == "" || limit <= 0 {
		return nil
	}
	remaining := []rune(value)
	chunks := make([]string, 0, (len(remaining)+limit-1)/limit)
	for telegramTextUnits(remaining) > limit {
		hardCut := telegramRuneCut(remaining, limit)
		cut := naturalMessageBreak(remaining[:hardCut], hardCut/2)
		chunk := strings.TrimSpace(string(remaining[:cut]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		rest := strings.TrimLeftFunc(string(remaining[cut:]), unicode.IsSpace)
		remaining = []rune(rest)
	}
	if tail := strings.TrimSpace(string(remaining)); tail != "" {
		chunks = append(chunks, tail)
	}
	return chunks
}

func telegramRuneCut(value []rune, limit int) int {
	units := 0
	for index, character := range value {
		width := utf16.RuneLen(character)
		if width < 1 {
			width = 1
		}
		if units+width > limit {
			return index
		}
		units += width
	}
	return len(value)
}

func telegramTextUnits(value []rune) int {
	units := 0
	for _, character := range value {
		width := utf16.RuneLen(character)
		if width < 1 {
			width = 1
		}
		units += width
	}
	return units
}

func naturalMessageBreak(value []rune, minimum int) int {
	for index := len(value) - 1; index >= minimum; index-- {
		if value[index] == '\n' && index > 0 && value[index-1] == '\n' {
			return index + 1
		}
	}
	for index := len(value) - 1; index >= minimum; index-- {
		if value[index] == '\n' {
			return index + 1
		}
	}
	for index := len(value) - 1; index >= minimum; index-- {
		if unicode.IsSpace(value[index]) {
			return index + 1
		}
	}
	return len(value)
}
