package bot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"strings"
	"time"

	"github.com/hyphentae/nyande-bot/internal/telegram"
	"github.com/hyphentae/nyande-bot/resources"
)

const (
	wordleMaxGuesses = 6
	wordleAbsent     = 0
	wordlePresent    = 1
	wordleCorrect    = 2
)

var wordleWords = []string{
	"actor", "admit", "adopt", "adult", "after", "again", "agent", "agree",
	"ahead", "alarm", "album", "alert", "alien", "alive", "allow", "alone",
	"alter", "among", "angel", "angle", "angry", "apple", "arena", "argue",
	"arise", "asset", "audio", "avoid", "award", "aware", "baker", "basic",
	"basin", "beach", "beard", "beast", "begin", "below", "bench", "berry",
	"birth", "black", "blade", "blame", "blank", "blast", "blend", "blind",
	"block", "bloom", "board", "bonus", "booth", "brain", "brake", "brave",
	"bread", "break", "brick", "bride", "brief", "bring", "broad", "brown",
	"brush", "build", "bunch", "cabin", "cable", "candy", "carry", "catch",
	"cause", "chain", "chair", "charm", "chart", "chase", "cheap", "check",
	"chest", "chief", "child", "claim", "class", "clean", "clear", "clerk",
	"click", "climb", "clock", "close", "cloud", "coach", "coast", "color",
	"coral", "count", "court", "cover", "crane", "cream", "crime", "cross",
	"crowd", "crown", "curve", "cycle", "dance", "dream", "drink", "drive",
	"earth", "eight", "empty", "enjoy", "enter", "equal", "event", "every",
	"exact", "faith", "false", "fault", "field", "fifth", "final", "first",
	"flame", "floor", "focus", "force", "frame", "fresh", "front", "fruit",
	"ghost", "giant", "given", "glass", "globe", "grace", "grade", "grain",
	"grand", "grant", "grape", "grass", "great", "green", "group", "guard",
	"guess", "guest", "guide", "happy", "heart", "heavy", "honey", "horse",
	"hotel", "house", "human", "ideal", "image", "index", "inner", "input",
	"issue", "jelly", "joint", "judge", "juice", "knife", "label", "large",
	"later", "laugh", "layer", "learn", "lemon", "light", "limit", "local",
	"logic", "lucky", "lunch", "magic", "major", "mango", "match", "maybe",
	"metal", "model", "money", "month", "mouse", "mouth", "movie", "music",
	"night", "noise", "north", "novel", "nurse", "ocean", "offer", "often",
	"olive", "order", "other", "paint", "panel", "paper", "party", "peace",
	"peach", "pearl", "phone", "piano", "piece", "pilot", "place", "plant",
	"plate", "point", "power", "press", "price", "pride", "prime", "prize",
	"proof", "proud", "queen", "quick", "quiet", "radio", "raise", "range",
	"reach", "ready", "reply", "right", "river", "robot", "rough", "round",
	"route", "royal", "rural", "scale", "scene", "scope", "score", "sense",
	"serve", "seven", "shape", "share", "sharp", "sheep", "shelf", "shell",
	"shift", "shine", "shirt", "shock", "short", "sight", "skill", "sleep",
	"small", "smart", "smile", "snake", "solid", "sound", "south", "space",
	"spare", "speak", "speed", "spoon", "sport", "stand", "start", "steam",
	"steel", "stick", "still", "stock", "stone", "store", "storm", "story",
	"study", "style", "sugar", "table", "taste", "teach", "thank", "their",
	"theme", "there", "thick", "thing", "think", "third", "tiger", "title",
	"toast", "today", "topic", "total", "touch", "tower", "track", "trade",
	"train", "treat", "trend", "trial", "trust", "truth", "under", "union",
	"unity", "until", "upper", "urban", "value", "video", "visit", "voice",
	"waste", "watch", "water", "wheel", "white", "whole", "woman", "world",
	"worth", "write", "wrong", "young", "zebra",
}

type wordleKey struct {
	ChatID int64
	UserID int64
}

type wordleGuess struct {
	Word   string
	Result [5]int
}

type wordleGame struct {
	Day      string
	Number   int
	Word     string
	Guesses  []wordleGuess
	Finished bool
	Won      bool
}

func (g *wordleGame) clone() wordleGame {
	copy := *g
	copy.Guesses = append([]wordleGuess(nil), g.Guesses...)
	return copy
}

func (b *Bot) startWordle(ctx context.Context, message *telegram.Message) error {
	if message.From == nil {
		return nil
	}
	day, number, word := currentWordle(b.cfg.LLMTimezone, time.Now())
	key := wordleKey{ChatID: message.Chat.ID, UserID: message.From.ID}

	b.wordleMu.Lock()
	game := b.wordleGames[key]
	created := game == nil || game.Day != day
	if created {
		game = &wordleGame{Day: day, Number: number, Word: word}
		b.wordleGames[key] = game
	}
	snapshot := game.clone()
	b.wordleMu.Unlock()

	caption := resources.Format("wordle.started", map[string]any{"number": snapshot.Number})
	if !created && len(snapshot.Guesses) > 0 && !snapshot.Finished {
		caption = resources.Format("wordle.resumed", map[string]any{
			"number": snapshot.Number, "attempt": len(snapshot.Guesses),
		})
	}
	if snapshot.Finished {
		caption = wordleFinalCaption(snapshot, message.From.DisplayName())
	}
	return b.sendWordleCard(ctx, message, snapshot, caption)
}

func (b *Bot) handleWordleGuess(ctx context.Context, message *telegram.Message, text string) (bool, error) {
	if message.From == nil || strings.TrimSpace(text) == "" {
		return false, nil
	}
	day, _, _ := currentWordle(b.cfg.LLMTimezone, time.Now())
	key := wordleKey{ChatID: message.Chat.ID, UserID: message.From.ID}

	b.wordleMu.Lock()
	game := b.wordleGames[key]
	if game == nil || game.Day != day || game.Finished {
		b.wordleMu.Unlock()
		return false, nil
	}
	guess, valid := normalizeWordleWord(text)
	if !valid {
		b.wordleMu.Unlock()
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("wordle.invalid"), message.MessageID, nil)
		return true, err
	}

	result := evaluateWordleGuess(game.Word, guess)
	game.Guesses = append(game.Guesses, wordleGuess{Word: guess, Result: result})
	game.Won = guess == game.Word
	game.Finished = game.Won || len(game.Guesses) >= wordleMaxGuesses
	snapshot := game.clone()
	b.wordleMu.Unlock()

	caption := resources.Format("wordle.progress", map[string]any{
		"number": snapshot.Number, "attempt": len(snapshot.Guesses),
	})
	if snapshot.Finished {
		caption = wordleFinalCaption(snapshot, message.From.DisplayName())
	}
	return true, b.sendWordleCard(ctx, message, snapshot, caption)
}

func wordleFinalCaption(game wordleGame, player string) string {
	key := "wordle.lost"
	if game.Won {
		key = "wordle.won"
	}
	return resources.Format(key, map[string]any{
		"number": game.Number, "player": player, "attempt": len(game.Guesses),
		"word": strings.ToUpper(game.Word),
	})
}

func (b *Bot) sendWordleCard(ctx context.Context, message *telegram.Message, game wordleGame, caption string) error {
	var avatar image.Image
	if game.Finished && message.From != nil {
		avatar = b.wordleAvatar(ctx, message.From.ID)
	}
	data, err := renderWordleCard(game, avatar)
	if err != nil {
		return fmt.Errorf("render wordle card: %w", err)
	}
	_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
		Kind: "photo", Name: fmt.Sprintf("wordle-%d.png", game.Number),
		MIME: "image/png", Data: data, Caption: caption,
	}, message.MessageID)
	return err
}

func (b *Bot) wordleAvatar(ctx context.Context, userID int64) image.Image {
	photos, err := b.telegram.GetUserProfilePhotos(ctx, userID, 1)
	if err != nil || len(photos.Photos) == 0 || len(photos.Photos[0]) == 0 {
		return nil
	}
	sizes := photos.Photos[0]
	data, _, err := b.telegram.DownloadFile(ctx, sizes[len(sizes)-1].FileID, 5*1024*1024)
	if err != nil {
		return nil
	}
	avatar, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return avatar
}

func normalizeWordleWord(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 5 {
		return "", false
	}
	for _, letter := range value {
		if letter < 'a' || letter > 'z' {
			return "", false
		}
	}
	return value, true
}

func evaluateWordleGuess(answer, guess string) [5]int {
	answerLetters := []rune(answer)
	guessLetters := []rune(guess)
	var result [5]int
	remaining := make(map[rune]int, 5)
	for index := range answerLetters {
		if guessLetters[index] == answerLetters[index] {
			result[index] = wordleCorrect
		} else {
			remaining[answerLetters[index]]++
		}
	}
	for index, letter := range guessLetters {
		if result[index] == wordleCorrect || remaining[letter] == 0 {
			continue
		}
		result[index] = wordlePresent
		remaining[letter]--
	}
	return result
}

func currentWordle(timezone string, now time.Time) (string, int, string) {
	location, err := time.LoadLocation(strings.TrimSpace(timezone))
	if err != nil {
		location = time.UTC
	}
	local := now.In(location)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	day := date.Format("2006-01-02")
	epoch := time.Date(2021, time.June, 19, 0, 0, 0, 0, time.UTC)
	number := int(date.Sub(epoch) / (24 * time.Hour))
	if number < 0 {
		number = 0
	}
	hash := sha256.Sum256([]byte("nyande-wordle:" + day))
	index := (int(hash[0])<<8 | int(hash[1])) % len(wordleWords)
	return day, number, wordleWords[index]
}

var (
	wordleBackground = color.RGBA{R: 0x12, G: 0x12, B: 0x13, A: 0xff}
	wordlePanel      = color.RGBA{R: 0x17, G: 0x17, B: 0x18, A: 0xff}
	wordleBorder     = color.RGBA{R: 0x3a, G: 0x3a, B: 0x3c, A: 0xff}
	wordleYellow     = color.RGBA{R: 0xb5, G: 0x9f, B: 0x3b, A: 0xff}
	wordleGreen      = color.RGBA{R: 0x53, G: 0x8d, B: 0x4e, A: 0xff}
	wordleWhite      = color.RGBA{R: 0xf4, G: 0xf4, B: 0xf4, A: 0xff}
)

func renderWordleCard(game wordleGame, avatar image.Image) ([]byte, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, 1024, 560))
	fill(canvas, canvas.Bounds(), wordleBackground)
	drawRoundedRect(canvas, image.Rect(150, 128, 874, 516), 34, wordleBorder)
	drawRoundedRect(canvas, image.Rect(154, 132, 870, 512), 30, wordlePanel)

	heading := fmt.Sprintf("WORDLE NO. %d", game.Number)
	drawBitmapText(canvas, (1024-bitmapTextWidth(heading, 7))/2, 48, heading, 7, wordleWhite)

	boardX := 366
	if game.Finished {
		boardX = 570
		drawWordleAvatar(canvas, 315, 322, 126, avatar)
	}
	drawWordleBoard(canvas, boardX, 148, game.Guesses)

	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func drawWordleBoard(destination *image.RGBA, x, y int, guesses []wordleGuess) {
	const tile = 52
	const gap = 8
	for row := 0; row < wordleMaxGuesses; row++ {
		for column := 0; column < 5; column++ {
			shade := wordleBorder
			if row < len(guesses) {
				switch guesses[row].Result[column] {
				case wordlePresent:
					shade = wordleYellow
				case wordleCorrect:
					shade = wordleGreen
				}
			}
			left := x + column*(tile+gap)
			top := y + row*(tile+gap)
			fill(destination, image.Rect(left, top, left+tile, top+tile), shade)
		}
	}
}

func drawWordleAvatar(destination *image.RGBA, centerX, centerY, radius int, avatar image.Image) {
	if avatar == nil || avatar.Bounds().Dx() == 0 || avatar.Bounds().Dy() == 0 {
		drawCircle(destination, centerX, centerY, radius, color.RGBA{R: 0x2f, G: 0x62, B: 0x99, A: 0xff})
		drawCircle(destination, centerX, centerY-37, 45, color.RGBA{R: 0xdf, G: 0xe8, B: 0xf2, A: 0xff})
		drawEllipse(destination, image.Rect(centerX-86, centerY+15, centerX+86, centerY+116), color.RGBA{R: 0xdf, G: 0xe8, B: 0xf2, A: 0xff})
		return
	}
	bounds := avatar.Bounds()
	sourceSize := bounds.Dx()
	sourceX := bounds.Min.X
	sourceY := bounds.Min.Y
	if bounds.Dy() < sourceSize {
		sourceSize = bounds.Dy()
		sourceX += (bounds.Dx() - sourceSize) / 2
	} else {
		sourceY += (bounds.Dy() - sourceSize) / 2
	}
	diameter := radius * 2
	for dy := -radius; dy < radius; dy++ {
		for dx := -radius; dx < radius; dx++ {
			if dx*dx+dy*dy > radius*radius {
				continue
			}
			sx := sourceX + (dx+radius)*sourceSize/diameter
			sy := sourceY + (dy+radius)*sourceSize/diameter
			destination.Set(centerX+dx, centerY+dy, avatar.At(sx, sy))
		}
	}
}

func fill(destination *image.RGBA, rectangle image.Rectangle, shade color.RGBA) {
	for y := rectangle.Min.Y; y < rectangle.Max.Y; y++ {
		for x := rectangle.Min.X; x < rectangle.Max.X; x++ {
			destination.SetRGBA(x, y, shade)
		}
	}
}

func drawRoundedRect(destination *image.RGBA, rectangle image.Rectangle, radius int, shade color.RGBA) {
	for y := rectangle.Min.Y; y < rectangle.Max.Y; y++ {
		for x := rectangle.Min.X; x < rectangle.Max.X; x++ {
			nearestX := min(max(x, rectangle.Min.X+radius), rectangle.Max.X-radius-1)
			nearestY := min(max(y, rectangle.Min.Y+radius), rectangle.Max.Y-radius-1)
			dx, dy := x-nearestX, y-nearestY
			if dx*dx+dy*dy <= radius*radius {
				destination.SetRGBA(x, y, shade)
			}
		}
	}
}

func drawCircle(destination *image.RGBA, centerX, centerY, radius int, shade color.RGBA) {
	for y := -radius; y <= radius; y++ {
		for x := -radius; x <= radius; x++ {
			if x*x+y*y <= radius*radius {
				destination.SetRGBA(centerX+x, centerY+y, shade)
			}
		}
	}
}

func drawEllipse(destination *image.RGBA, rectangle image.Rectangle, shade color.RGBA) {
	centerX := float64(rectangle.Min.X+rectangle.Max.X) / 2
	centerY := float64(rectangle.Min.Y+rectangle.Max.Y) / 2
	radiusX := float64(rectangle.Dx()) / 2
	radiusY := float64(rectangle.Dy()) / 2
	for y := rectangle.Min.Y; y < rectangle.Max.Y; y++ {
		for x := rectangle.Min.X; x < rectangle.Max.X; x++ {
			dx := (float64(x) - centerX) / radiusX
			dy := (float64(y) - centerY) / radiusY
			if dx*dx+dy*dy <= 1 {
				destination.SetRGBA(x, y, shade)
			}
		}
	}
}

func bitmapTextWidth(value string, scale int) int {
	if len(value) == 0 {
		return 0
	}
	return len([]rune(value))*6*scale - scale
}

func drawBitmapText(destination *image.RGBA, x, y int, value string, scale int, shade color.RGBA) {
	cursor := x
	for _, character := range strings.ToUpper(value) {
		glyph := wordleGlyphs[character]
		for row, pixels := range glyph {
			for column, pixel := range pixels {
				if pixel == '1' {
					fill(destination, image.Rect(cursor+column*scale, y+row*scale, cursor+(column+1)*scale, y+(row+1)*scale), shade)
				}
			}
		}
		cursor += 6 * scale
	}
}

var wordleGlyphs = map[rune][7]string{
	' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
	'.': {"00000", "00000", "00000", "00000", "00000", "00110", "00110"},
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	'6': {"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	'D': {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'L': {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'N': {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
	'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'R': {"11110", "10001", "10001", "11110", "10100", "10010", "10001"},
	'W': {"10001", "10001", "10001", "10101", "10101", "10101", "01010"},
}
