package bot

import (
	"archive/zip"
	"bytes"
	"image/png"
	"path"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/quotes"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

func TestQuoteEmojiSequencesAndPresentation(t *testing.T) {
	for _, test := range []struct{ text, file string }{
		{"🇰🇿", "1f1f0-1f1ff"}, {"🇷🇺", "1f1f7-1f1fa"},
		{"👩🏽‍💻", "1f469-1f3fd-200d-1f4bb"},
		{"👨‍👩‍👧‍👦", "1f468-200d-1f469-200d-1f467-200d-1f466"},
		{"🏳️‍🌈", "1f3f3-fe0f-200d-1f308"}, {"🏳‍🌈", "1f3f3-fe0f-200d-1f308"},
		{"1️⃣", "31-20e3"}, {"1⃣", "31-20e3"}, {"❤", "2764"}, {"❤️", "2764"},
		{"©️", "a9"}, {"🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f", "1f3f4-e0067-e0062-e0065-e006e-e0067-e007f"},
	} {
		text, author, files := prepareQuoteEmoji(test.text, test.text)
		marker, _ := utf8.DecodeRuneInString(text)
		if utf8.RuneCountInString(text) != 1 || text != author || len(files) != 1 || files[marker] == nil || path.Base(files[marker].Name) != test.file+".png" {
			t.Fatalf("sequence %q was split or replaced incorrectly: %q, %q, %#v", test.text, text, author, files)
		}
	}
	for input, want := range map[string]string{"© ® ™": "© ® ™", "©\ufe0e": "©", "A\u200d\ufe0f\ufe0e\U000e0067Z": "AZ"} {
		text, author, files := prepareQuoteEmoji(input, input)
		if text != want || author != want || len(files) != 0 {
			t.Fatalf("presentation %q: %q, %q, %#v", input, text, author, files)
		}
	}
	text, _, files := prepareQuoteEmoji("😎\u200d😎\ufe0f", "")
	if utf8.RuneCountInString(text) != 2 || len(files) != 1 || []rune(text)[0] != []rune(text)[1] {
		t.Fatalf("unknown joined sequence left control glyphs: %q, %#v", text, files)
	}
	text, author, files := prepareQuoteEmoji("\U000f0000😎", "\U000f0001😎")
	if utf8.RuneCountInString(text) != 2 || utf8.RuneCountInString(author) != 2 || []rune(text)[0] != 0xf0000 || []rune(author)[0] != 0xf0001 || []rune(text)[1] != []rune(author)[1] || files[0xf0000] != nil || files[0xf0001] != nil || len(files) != 1 {
		t.Fatalf("input private-use runes collided with emoji: %q, %q, %#v", text, author, files)
	}
}

func TestQuoteEmojiMetricsAndWrapping(t *testing.T) {
	text, _, files := prepareQuoteEmoji(strings.Repeat("👩🏽‍💻", 15), "")
	marker, _ := utf8.DecodeRuneInString(text)
	for _, size := range []int{24, 36} {
		face, err := newQuoteTextFace(size, files)
		if err != nil {
			t.Fatal(err)
		}
		defer face.Close()
		advance, ok := face.GlyphAdvance(marker)
		dr, _, _, drawnAdvance, drawn := face.Glyph(fixed.P(0, size), marker)
		_, boundedAdvance, bounded := face.GlyphBounds(marker)
		if !ok || !drawn || !bounded || !dr.Empty() || advance <= 0 || drawnAdvance != advance || boundedAdvance != advance || font.MeasureString(face, text) != advance*15 || face.Kern('A', marker) != 0 || face.Kern(marker, 'V') != 0 {
			t.Fatal("emoji drawing and wrapping metrics disagree")
		}
		lines := wrapTelegramText(text, face, (advance * 3).Ceil(), 20)
		if len(lines) != 5 || strings.Join(lines, "") != text {
			t.Fatalf("emoji were split or lost: %q", lines)
		}
		for _, line := range lines {
			if utf8.RuneCountInString(line) != 3 {
				t.Fatalf("compound emoji lost atomic width: %q", line)
			}
		}
	}
}

func TestQuoteEmojiRenderTextAuthorAndLongTail(t *testing.T) {
	for _, quote := range []quotes.Quote{
		{Text: "Кот 😎", Author: "Автор"},
		{Text: "Кот", Author: "Автор 😎"},
		{Text: strings.Repeat("Я", quotes.MaxTextRunes-1) + "😎", Author: "Автор"},
	} {
		data, err := renderQuoteCard(quote, nil)
		if err != nil {
			t.Fatal(err)
		}
		card, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		colored := 0
		for y := 0; y < card.Bounds().Dy(); y++ {
			for x := 0; x < card.Bounds().Dx(); x++ {
				r, g, b, _ := card.At(x, y).RGBA()
				// The dark background and antialiased white/black lettering cannot be saturated.
				if int(max(r, g, b))-min(int(r), min(int(g), int(b))) > 60*257 && max(r, g, b) > 110*257 {
					colored++
				}
			}
		}
		if colored < 20 {
			t.Fatalf("emoji bitmap missing (text runes=%d, author=%q): %d colored pixels", utf8.RuneCountInString(quote.Text), quote.Author, colored)
		}
	}
}

func TestQuoteEmojiArchiveContainsValidImages(t *testing.T) {
	archive, err := zip.NewReader(bytes.NewReader(quoteEmojiData), int64(len(quoteEmojiData)))
	if err != nil {
		t.Fatal(err)
	}
	images, notices := 0, 0
	for _, file := range archive.File {
		if file.Name == "ATTRIBUTION.txt" || file.Name == "LICENSE-GRAPHICS" {
			if file.UncompressedSize64 > 0 {
				notices++
			}
		}
		if !strings.HasSuffix(file.Name, ".png") {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(reader)
		reader.Close()
		if err != nil || cfg.Width != 72 || cfg.Height != 72 {
			t.Fatalf("invalid emoji %s: %#v, %v", file.Name, cfg, err)
		}
		images++
	}
	if images != 4009 || notices != 2 {
		t.Fatalf("incomplete pinned archive: %d images, %d notices", images, notices)
	}
}
