package bot

import (
	"bytes"
	"image"
	"image/color"
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
	for i, content := range []string{"Я не опоздал. Я дал вам время соскучиться.", strings.Repeat("Ш", quotes.MaxTextRunes), strings.Repeat("Кот и код. ", 100)} {
		data, err := renderQuoteCard(quotes.Quote{ID: 12, Author: "Маша", Text: content, Date: 1700000000}, nil)
		if err != nil {
			t.Fatal(err)
		}
		card, format, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if format != "png" || card.Bounds().Dx() != 800 || card.Bounds().Dy() < 800 || card.Bounds().Dy() > 4000 || i == 0 && card.Bounds().Dy() != 800 {
			t.Fatalf("invalid card dimensions: %v", card.Bounds())
		}
		if i == 1 {
			changed, err := renderQuoteCard(quotes.Quote{ID: 12, Author: "Маша", Text: strings.Repeat("Ш", quotes.MaxTextRunes-1) + "Я", Date: 1700000000}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(data, changed) {
				t.Fatal("the last character of a 1200-character quote was clipped")
			}
		}
	}
}

func TestQuoteCardUsesAvatarAsFullBackground(t *testing.T) {
	avatar := image.NewRGBA(image.Rect(30, 40, 158, 104))
	fill(avatar, avatar.Bounds(), color.RGBA{20, 220, 40, 255})
	fill(avatar, image.Rect(62, 40, 126, 104), color.RGBA{220, 50, 70, 255})
	data, err := renderQuoteCard(quotes.Quote{Author: "Маша", Text: "Кот и код."}, avatar)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := renderQuoteCard(quotes.Quote{Author: "Маша", Text: "«Кот и код.»"}, avatar)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, quoted) {
		t.Fatal("existing quote marks should not be duplicated")
	}
	card, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []image.Point{{0, 0}, {799, 0}, {0, 799}, {799, 799}} {
		shade := color.RGBAModel.Convert(card.At(point.X, point.Y)).(color.RGBA)
		if shade.A != 255 || shade.R <= shade.G*2 || shade.R >= 220 {
			t.Fatalf("avatar should cover the card and be dimmed: %v at %v", shade, point)
		}
	}
	var white, black int
	for y := 0; y < 800; y++ {
		for x := 0; x < 800; x++ {
			shade := color.RGBAModel.Convert(card.At(x, y)).(color.RGBA)
			if shade.R == 255 && shade.G == 255 && shade.B == 255 {
				white++
			}
			if shade.R == 0 && shade.G == 0 && shade.B == 0 {
				black++
			}
		}
	}
	if white == 0 || black == 0 {
		t.Fatal("quote needs white lettering with a black outline")
	}
}
