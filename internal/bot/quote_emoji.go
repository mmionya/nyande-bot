package bot

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/png"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Twemoji graphics, CC-BY-4.0; license and attribution are inside the archive.
// ponytail: coverage follows this pinned asset version; update it for newer emoji.
//
//go:embed assets/twemoji-17.0.3.zip
var quoteEmojiData []byte

type quoteEmojiNode struct {
	next map[rune]*quoteEmojiNode
	file *zip.File
}

var quoteEmojiRoot = loadQuoteEmoji()

func loadQuoteEmoji() *quoteEmojiNode {
	archive, err := zip.NewReader(bytes.NewReader(quoteEmojiData), int64(len(quoteEmojiData)))
	if err != nil {
		panic(err)
	}
	root := &quoteEmojiNode{}
	for _, file := range archive.File {
		if !strings.HasSuffix(file.Name, ".png") {
			continue
		}
		node := root
		for _, code := range strings.Split(strings.TrimSuffix(path.Base(file.Name), ".png"), "-") {
			value, err := strconv.ParseInt(code, 16, 32)
			if err != nil || !utf8.ValidRune(rune(value)) {
				panic("invalid bundled emoji name: " + file.Name)
			}
			if value == 0xfe0f {
				continue
			}
			if node.next == nil {
				node.next = make(map[rune]*quoteEmojiNode)
			}
			if node.next[rune(value)] == nil {
				node.next[rune(value)] = &quoteEmojiNode{}
			}
			node = node.next[rune(value)]
		}
		if node.file != nil {
			panic("duplicate bundled emoji: " + file.Name)
		}
		node.file = file
	}
	return root
}

func matchQuoteEmoji(value string) (*zip.File, int) {
	node := quoteEmojiRoot
	var file *zip.File
	end := 0
	for offset, r := range value {
		if r == '\ufe0e' { // Explicit text presentation.
			return nil, 0
		}
		if r == '\ufe0f' {
			if end == offset {
				end = offset + utf8.RuneLen(r)
			}
			continue
		}
		node = node.next[r]
		if node == nil {
			break
		}
		if node.file != nil {
			file, end = node.file, offset+utf8.RuneLen(r)
		}
	}
	return file, end
}

func prepareQuoteEmoji(text, author string) (string, string, map[rune]*zip.File) {
	used := make(map[rune]bool)
	for _, r := range text + author {
		used[r] = true
	}
	files := make(map[rune]*zip.File)
	markers := make(map[*zip.File]rune)
	next := rune(0xf0000)
	encode := func(value string) string {
		var result strings.Builder
		for len(value) > 0 {
			r, length := utf8.DecodeRuneInString(value)
			if r == '\u200d' || r == '\ufe0e' || r == '\ufe0f' || r >= 0xe0020 && r <= 0xe007f {
				// Unmatched presentation controls must not become missing-glyph boxes.
				value = value[length:]
				continue
			}
			file, end := matchQuoteEmoji(value)
			if (r == '©' || r == '®' || r == '™') && !strings.HasPrefix(value[length:], "\ufe0f") {
				file = nil
			}
			if file == nil {
				result.WriteRune(r)
				value = value[length:]
				continue
			}
			marker, exists := markers[file]
			if !exists {
				for used[next] {
					next++
				}
				marker = next
				next++
				markers[file], files[marker] = marker, file
			}
			result.WriteRune(marker)
			value = value[end:]
		}
		return result.String()
	}
	return encode(text), encode(author), files
}

// One marker per complete emoji keeps the existing rune-based wrapping atomic.
type quoteTextFace struct {
	font.Face
	size   int
	files  map[rune]*zip.File
	images map[*zip.File]image.Image
}

func newQuoteTextFace(size int, files map[rune]*zip.File) (*quoteTextFace, error) {
	face, err := opentype.NewFace(quoteFont, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	return &quoteTextFace{Face: face, size: size, files: files, images: make(map[*zip.File]image.Image)}, nil
}

func (f *quoteTextFace) Glyph(dot fixed.Point26_6, r rune) (image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool) {
	if f.files[r] != nil {
		return image.Rectangle{}, nil, image.Point{}, fixed.I(f.size + 2), true
	}
	return f.Face.Glyph(dot, r)
}

func (f *quoteTextFace) GlyphAdvance(r rune) (fixed.Int26_6, bool) {
	if f.files[r] != nil {
		return fixed.I(f.size + 2), true
	}
	return f.Face.GlyphAdvance(r)
}

func (f *quoteTextFace) GlyphBounds(r rune) (fixed.Rectangle26_6, fixed.Int26_6, bool) {
	if f.files[r] != nil {
		return fixed.Rectangle26_6{Min: fixed.P(1, -f.size+f.size/8), Max: fixed.P(f.size+1, f.size/8)}, fixed.I(f.size + 2), true
	}
	return f.Face.GlyphBounds(r)
}

func (f *quoteTextFace) Kern(left, right rune) fixed.Int26_6 {
	if f.files[left] != nil || f.files[right] != nil {
		return 0
	}
	return f.Face.Kern(left, right)
}

func (f *quoteTextFace) drawEmoji(canvas *image.RGBA, x, baseline int, text string) error {
	pen, previous := fixed.P(x, baseline), rune(-1)
	for _, r := range text {
		if previous >= 0 {
			pen.X += f.Kern(previous, r)
		}
		if file := f.files[r]; file != nil {
			icon := f.images[file]
			if icon == nil {
				reader, err := file.Open()
				if err != nil {
					return err
				}
				source, err := png.Decode(reader)
				reader.Close()
				if err != nil {
					return fmt.Errorf("decode quote emoji %s: %w", file.Name, err)
				}
				// Cache only the small images used by this face during this render.
				scaled := image.NewRGBA(image.Rect(0, 0, f.size, f.size))
				draw.CatmullRom.Scale(scaled, scaled.Bounds(), source, source.Bounds(), draw.Src, nil)
				icon, f.images[file] = scaled, scaled
			}
			origin := image.Pt(pen.X.Round()+1, baseline-f.size+f.size/8)
			draw.Draw(canvas, image.Rectangle{Min: origin, Max: origin.Add(icon.Bounds().Size())}, icon, icon.Bounds().Min, draw.Over)
		}
		advance, _ := f.GlyphAdvance(r)
		pen.X += advance
		previous = r
	}
	return nil
}
