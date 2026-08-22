package bot

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestSplitTelegramMessagePreservesLongUnbrokenText(t *testing.T) {
	input := strings.Repeat("я", telegramMessageLimit*2+17)
	chunks := splitTelegramMessage(input, telegramMessageLimit)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	if strings.Join(chunks, "") != input {
		t.Fatal("split message lost content")
	}
	for _, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > telegramMessageLimit {
			t.Fatalf("chunk is too long: %d", utf8.RuneCountInString(chunk))
		}
	}
}

func TestSplitTelegramMessageCountsEmojiSafely(t *testing.T) {
	input := strings.Repeat("😺", 3000)
	chunks := splitTelegramMessage(input, telegramMessageLimit)
	if len(chunks) != 2 || strings.Join(chunks, "") != input {
		t.Fatalf("unexpected emoji split: chunks=%d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > telegramMessageLimit {
			t.Fatal("emoji chunk exceeds Telegram's UTF-16-safe limit")
		}
	}
}

func TestSplitTelegramMessagePrefersNaturalBoundary(t *testing.T) {
	input := strings.Repeat("word ", 30) + "\n\n" + strings.Repeat("tail ", 30)
	chunks := splitTelegramMessage(input, 180)
	if len(chunks) < 2 || !strings.HasSuffix(chunks[0], "word") {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
	if !strings.Contains(strings.Join(chunks, " "), "tail") {
		t.Fatal("tail content was lost")
	}
}

func TestSplitTelegramMessageRejectsEmptyInput(t *testing.T) {
	if chunks := splitTelegramMessage("   ", telegramMessageLimit); len(chunks) != 0 {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}
