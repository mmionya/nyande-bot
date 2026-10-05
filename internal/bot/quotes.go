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
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

const quoteHelp = "Цитатник этого чата:\n/quote — ответом на сообщение: сохранить и сделать карточку\n/quote random — случайная цитата\n/quote list — последние 10 цитат\n/quote 12 — цитата по номеру\n/quote delete 12 — удалить (автор, сохранивший или администратор)\n/quote trigger — показать слово-триггер\n/quote trigger цитата — задать слово для цитирования ответом\n/quote trigger off — отключить триггер\n\nВ группах триггер настраивают администраторы. Можно цитировать текст и подписи к медиа, до 1200 символов."

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
	case len(args) > 0 && strings.EqualFold(args[0], "trigger"):
		if len(args) > 2 {
			return say(quoteHelp)
		}
		if len(args) == 1 {
			trigger, err := b.quotes.Trigger(ctx, message.Chat.ID)
			if err != nil {
				return err
			}
			if trigger == "" {
				return say("Триггер цитат отключён. Задать: /quote trigger цитата")
			}
			return say(fmt.Sprintf("Триггер цитат: %s. Ответь этим словом на сообщение; регистр не важен.", trigger))
		}
		admin, err := b.isAdmin(ctx, message)
		if err != nil {
			return err
		}
		if !admin {
			return say("Настраивать триггер цитат могут только администраторы чата.")
		}
		trigger := args[1]
		if strings.EqualFold(trigger, silentModerationTrigger) {
			return say("Это слово занято другой командой бота. Выбери другой триггер.")
		}
		if strings.EqualFold(trigger, "off") {
			trigger = ""
		}
		if err := b.quotes.SetTrigger(ctx, message.Chat.ID, trigger); err != nil {
			if errors.Is(err, quotes.ErrInvalidTrigger) {
				return say("Триггер — одно слово до 32 символов: буквы, цифры, дефис или подчёркивание.")
			}
			return err
		}
		if trigger == "" {
			return say("Триггер цитат отключён. Команда /quote продолжает работать.")
		}
		return say(fmt.Sprintf("Триггер сохранён: %s. Ответь только этим словом на сообщение, и я сделаю цитату. Регистр не важен.", trigger))
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

func (b *Bot) handleQuoteTrigger(ctx context.Context, message *telegram.Message) (bool, error) {
	if b.quotes == nil || message.ReplyToMessage == nil || message.From == nil || message.From.IsBot || message.SenderChat != nil {
		return false, nil
	}
	trigger, err := b.quotes.Trigger(ctx, message.Chat.ID)
	if err != nil {
		return true, err
	}
	if trigger == "" || !strings.EqualFold(strings.TrimSpace(message.ContentText()), trigger) {
		return false, nil
	}
	return true, b.quoteCommand(ctx, message, "")
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
	const width, margin = 800, 128
	name, err := opentype.NewFace(tomatoBoldFont, &opentype.FaceOptions{Size: 36, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer name.Close()
	text := strings.TrimSpace(q.Text)
	if !strings.HasPrefix(text, "«") || !strings.HasSuffix(text, "»") {
		text = "«" + text + "»"
	}
	author := wrapTelegramText("© "+q.Author, name, width-2*margin, utf8.RuneCountInString(q.Author)+2)
	nameHeight := name.Metrics().Height.Ceil()
	var body font.Face
	var lines []string
	var lineHeight, blockHeight int
	for size := 36; ; size -= 2 {
		body, err = opentype.NewFace(tomatoBoldFont, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
		if err != nil {
			return nil, err
		}
		lines = wrapTelegramText(text, body, width-2*margin, quotes.MaxTextRunes+2)
		lineHeight = body.Metrics().Height.Ceil()
		blockHeight = len(lines)*lineHeight + 24 + len(author)*nameHeight
		if blockHeight <= width-2*margin || size == 24 {
			break
		}
		body.Close()
	}
	defer body.Close()
	height := max(width, blockHeight+2*margin)
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(canvas, canvas.Bounds(), color.RGBA{30, 35, 44, 255})
	if avatar != nil && !avatar.Bounds().Empty() {
		source := avatar.Bounds()
		if source.Dx()*height > source.Dy()*width {
			cropWidth := max(1, source.Dy()*width/height)
			source.Min.X += (source.Dx() - cropWidth) / 2
			source.Max.X = source.Min.X + cropWidth
		} else {
			cropHeight := max(1, source.Dx()*height/width)
			source.Min.Y += (source.Dy() - cropHeight) / 2
			source.Max.Y = source.Min.Y + cropHeight
		}
		// ponytail: Fixed-scale resampling approximates a small blur; use a convolution
		// filter if the blur radius needs to be independent of the scaling filter.
		softened := image.NewRGBA(image.Rect(0, 0, width/6, height/6))
		draw.CatmullRom.Scale(softened, softened.Bounds(), avatar, source, draw.Src, nil)
		draw.BiLinear.Scale(canvas, canvas.Bounds(), softened, softened.Bounds(), draw.Over, nil)
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.NRGBA{A: 72}), image.Point{}, draw.Over)
	}
	outlinedText := func(face font.Face, baseline int, text string) {
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				if dx*dx+dy*dy <= 4 {
					drawText(canvas, face, margin+dx, baseline+dy, text, color.Black)
				}
			}
		}
		drawText(canvas, face, margin, baseline, text, color.White)
	}
	top := (height - blockHeight) / 2
	for i, line := range lines {
		outlinedText(body, top+body.Metrics().Ascent.Ceil()+i*lineHeight, line)
	}
	for i, line := range author {
		outlinedText(name, top+len(lines)*lineHeight+24+name.Metrics().Ascent.Ceil()+i*nameHeight, line)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
