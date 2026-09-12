package bot

import (
	"bytes"
	"image"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/quotes"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestQuotePreservesOriginalAuthor(t *testing.T) {
	forwarder := &telegram.User{ID: 5, FirstName: "Переславший"}
	for _, test := range []struct {
		origin *telegram.MessageOrigin
		name   string
		id     int64
	}{
		{nil, "Переславший", 5},
		{&telegram.MessageOrigin{SenderUser: &telegram.User{ID: 9, FirstName: "Автор"}}, "Автор", 9},
		{&telegram.MessageOrigin{SenderUserName: "Скрытый автор"}, "Скрытый автор", 0},
		{&telegram.MessageOrigin{Chat: &telegram.Chat{Title: "Канал"}}, "Канал", 0},
	} {
		q := quoteFromMessage(-100, 8, &telegram.Message{MessageID: 17, From: forwarder, ForwardOrigin: test.origin, Caption: "Подпись к видео"})
		if q.Author != test.name || q.AuthorID != test.id || q.Text != "Подпись к видео" || q.ChatID != -100 || q.SavedBy != 8 || q.MessageID != 17 {
			t.Fatalf("wrong attribution: %#v", q)
		}
	}
	q := quoteFromMessage(1, 2, &telegram.Message{From: forwarder, SenderChat: &telegram.Chat{Title: "Анонимный администратор"}})
	if q.AuthorID != 0 || q.Author != "Анонимный администратор" {
		t.Fatalf("sender chat: %#v", q)
	}
}

func TestQuoteCardHandlesLongTextAndMissingAvatar(t *testing.T) {
	for _, content := range []string{"Я не опоздал. Я дал вам время соскучиться.", strings.Repeat("Ш", quotes.MaxTextRunes), strings.Repeat("Кот и код. ", 100)} {
		data, err := renderQuoteCard(quotes.Quote{ID: 12, Author: "Маша", Text: content, Date: 1700000000}, nil)
		if err != nil {
			t.Fatal(err)
		}
		card, format, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if format != "png" || card.Bounds().Dx() != 800 || card.Bounds().Dy() < 360 || card.Bounds().Dy() > 4000 {
			t.Fatalf("invalid card dimensions: %v", card.Bounds())
		}
	}
}
