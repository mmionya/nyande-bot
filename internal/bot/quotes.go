package bot

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/quotes"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

const quoteHelp = "Цитатник этого чата:\n/quote — ответом на сообщение: сохранить и сделать карточку\n/quote random — случайная цитата\n/quote list — последние 10 цитат\n/quote 12 — цитата по номеру\n/quote delete 12 — удалить (автор, сохранивший или администратор)\n\nМожно цитировать текст и подписи к медиа, до 1200 символов."

func (b *Bot) quoteCommand(ctx context.Context, message *telegram.Message, arguments string) error {
	say := func(text string) error {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, nil)
		return err
	}
	if b.quotes == nil {
		return say("Цитатник недоступен.")
	}
	args := strings.Fields(arguments)
	var q quotes.Quote
	var err error
	switch {
	case len(args) == 0 && message.ReplyToMessage != nil:
		target := message.ReplyToMessage
		if strings.TrimSpace(target.ContentText()) == "" {
			return say("Ответь /quote на сообщение с текстом или подписью к медиа.")
		}
		if utf8.RuneCountInString(strings.TrimSpace(target.ContentText())) > quotes.MaxTextRunes {
			return say("Для карточки нужно не больше 1200 символов. Выбери сообщение покороче.")
		}
		if message.From == nil || message.SenderChat != nil {
			return say("Сохрани цитату от своего имени, а не от имени канала.")
		}
		q = quoteFromMessage(message.Chat.ID, message.From.ID, target)
		q, err = b.quotes.Add(ctx, q)
	case len(args) == 1 && strings.EqualFold(args[0], "random"):
		q, err = b.quotes.Random(ctx, message.Chat.ID)
	case len(args) == 1 && strings.EqualFold(args[0], "list"):
		items, listErr := b.quotes.List(ctx, message.Chat.ID)
		if listErr != nil {
			return listErr
		}
		if len(items) == 0 {
			return say("Здесь пока нет цитат. Сохрани первую командой /quote в ответ на сообщение.")
		}
		var lines []string
		for _, item := range items {
			text := []rune(strings.Join(strings.Fields(item.Text), " "))
			if len(text) > 90 {
				text = append(text[:89], '…')
			}
			author := []rune(strings.Join(strings.Fields(item.Author), " "))
			if len(author) > 50 {
				author = append(author[:49], '…')
			}
			lines = append(lines, fmt.Sprintf("#%d · %s\n%s", item.ID, string(author), string(text)))
		}
		return say("Последние цитаты:\n\n" + strings.Join(lines, "\n\n") + "\n\nОткрыть: /quote номер")
	case len(args) == 2 && strings.EqualFold(args[0], "delete"):
		id, parseErr := strconv.ParseInt(args[1], 10, 64)
		if parseErr != nil || id < 1 {
			return say(quoteHelp)
		}
		if message.From == nil || message.SenderChat != nil {
			return say("Удалить цитату можно от своего имени.")
		}
		q, err = b.quotes.Get(ctx, message.Chat.ID, id)
		if errors.Is(err, sql.ErrNoRows) {
			return say("Такой цитаты в этом чате нет.")
		}
		if err != nil {
			return err
		}
		admin := false
		if q.AuthorID != message.From.ID && q.SavedBy != message.From.ID {
			admin, err = b.isAdmin(ctx, message)
			if err != nil {
				return err
			}
		}
		deleted, deleteErr := b.quotes.Delete(ctx, message.Chat.ID, id, message.From.ID, admin)
		if deleteErr != nil {
			return deleteErr
		}
		if !deleted {
			return say("Удалить цитату может её автор, сохранивший её участник или администратор.")
		}
		return say(fmt.Sprintf("Цитата #%d удалена.", id))
	case len(args) == 1:
		id, parseErr := strconv.ParseInt(args[0], 10, 64)
		if parseErr != nil || id < 1 {
			return say(quoteHelp)
		}
		q, err = b.quotes.Get(ctx, message.Chat.ID, id)
	default:
		return say(quoteHelp)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return say("Цитата не найдена. Сохрани сообщение командой /quote или посмотри /quote list.")
	}
	if err != nil {
		return err
	}
	var avatar image.Image
	if q.AuthorID > 0 {
		avatarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		avatar = b.wordleAvatar(avatarCtx, q.AuthorID)
		cancel()
	}
	data, err := renderQuoteCard(q, avatar)
	if err != nil {
		return err
	}
	_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
		Kind: "photo", Name: fmt.Sprintf("quote-%d.png", q.ID), MIME: "image/png", Data: data,
		Caption: fmt.Sprintf("Цитата #%d · /quote random", q.ID),
	}, message.MessageID)
	return err
}

func quoteFromMessage(chatID, savedBy int64, m *telegram.Message) quotes.Quote {
	q := quotes.Quote{ChatID: chatID, SavedBy: savedBy, MessageID: m.MessageID, Date: m.Date, Text: m.ContentText(), Author: "Неизвестный автор"}
	if m.From != nil {
		q.AuthorID, q.Author = m.From.ID, m.From.DisplayName()
	}
	if m.SenderChat != nil {
		q.AuthorID, q.Author = 0, m.SenderChat.Title
	}
	if origin := m.ForwardOrigin; origin != nil {
		q.AuthorID, q.Author, q.Date = 0, "Неизвестный автор", origin.Date
		switch {
		case origin.SenderUser != nil:
			q.AuthorID, q.Author = origin.SenderUser.ID, origin.SenderUser.DisplayName()
		case origin.SenderUserName != "":
			q.Author = origin.SenderUserName
		case origin.SenderChat != nil:
			q.Author = origin.SenderChat.Title
		case origin.Chat != nil:
			q.Author = origin.Chat.Title
		}
	}
	return q
}

func renderQuoteCard(q quotes.Quote, avatar image.Image) ([]byte, error) {
	body, err := opentype.NewFace(tomatoRegularFont, &opentype.FaceOptions{Size: 28, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer body.Close()
	name, err := opentype.NewFace(tomatoBoldFont, &opentype.FaceOptions{Size: 24, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer name.Close()
	label, err := opentype.NewFace(tomatoRegularFont, &opentype.FaceOptions{Size: 16, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer label.Close()
	lines := wrapTelegramText(q.Text, body, 656, quotes.MaxTextRunes)
	height := max(360, 270+len(lines)*39)
	canvas := image.NewRGBA(image.Rect(0, 0, 800, height))
	background := color.RGBA{19, 21, 32, 255}
	accent := color.RGBA{188, 168, 255, 255}
	muted := color.RGBA{157, 164, 184, 255}
	fill(canvas, canvas.Bounds(), background)
	drawRoundedRect(canvas, image.Rect(24, 24, 776, height-24), 24, color.RGBA{31, 34, 49, 255})
	fill(canvas, image.Rect(52, 60, 56, height-60), accent)
	drawText(canvas, label, 76, 77, "НЯНДЕ / ЦИТАТНИК", accent)
	drawText(canvas, label, 660, 77, fmt.Sprintf("#%d", q.ID), muted)
	for i, line := range lines {
		drawText(canvas, body, 76, 132+i*39, line, color.RGBA{241, 242, 248, 255})
	}
	centerY := height - 103
	if avatar != nil {
		drawWordleAvatar(canvas, 106, centerY, 30, avatar)
	} else {
		drawCircle(canvas, 106, centerY, 30, color.RGBA{86, 70, 122, 255})
		initial := "?"
		if runes := []rune(strings.TrimSpace(q.Author)); len(runes) > 0 {
			initial = strings.ToUpper(string(runes[0]))
		}
		drawText(canvas, name, 106-font.MeasureString(name, initial).Ceil()/2, centerY+8, initial, color.White)
	}
	author := wrapTelegramText(q.Author, name, 570, 1)
	if len(author) > 0 {
		drawText(canvas, name, 154, centerY-2, author[0], accent)
	}
	stamp := "дата неизвестна"
	if q.Date > 0 {
		stamp = time.Unix(q.Date, 0).UTC().Format("02.01.2006")
	}
	drawText(canvas, label, 154, centerY+23, stamp, muted)
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
