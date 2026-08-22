package bot

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hyphentae/nyande-bot/internal/llm"
	"github.com/hyphentae/nyande-bot/internal/telegram"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	tomatoGIFWidth  = 720
	tomatoGIFHeight = 480
	tomatoHeader    = 58
	tomatoComposer  = 54
)

// Tomato throw source: https://tenor.com/view/tomato-throw-green-screen-gif-meme-gif-11542842272251393511
//
//go:embed assets/tomato-throw.gif
var tomatoThrowGIF []byte

var (
	tomatoRegularFont = mustOpenTypeFont(goregular.TTF)
	tomatoBoldFont    = mustOpenTypeFont(gobold.TTF)
	tomatoPalette     = makeTelegramPalette()
)

type telegramFaces struct {
	regular font.Face
	small   font.Face
	bold    font.Face
	header  font.Face
}

func (b *Bot) tomatoTool(message *telegram.Message) llm.Tool {
	sent := false
	return llm.Tool{
		Name:        "throw_tomatoes",
		Description: "Create and send an animated GIF showing the replied-to message inside a realistic Telegram chat interface while a tomato is thrown over it. Call this when the user asks to throw tomatoes at, pelt, boo, or tomato-splat a message, including Russian requests such as 'закидай помидорами' or 'кинь в это помидор'. The tool targets the replied-to message automatically and takes no arguments.",
		Parameters: map[string]any{
			"type": "object", "properties": map[string]any{}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, _ map[string]string) (string, error) {
			if sent {
				return "The tomato animation was already sent. Do not call this tool again.", nil
			}
			target := message.ReplyToMessage
			if target == nil {
				return "There is no replied-to message. Ask the user to reply to the target message and repeat the request.", nil
			}
			animation, err := renderTelegramTomatoGIF(target, userID(message))
			if err != nil {
				return "", err
			}
			_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{
				Kind: "animation", Name: "telegram-tomato.gif", MIME: "image/gif", Data: animation,
			}, target.MessageID)
			if err != nil {
				return "", err
			}
			sent = true
			return "A tomato was composited over the target message in a Telegram chat animation and sent. Briefly acknowledge it without calling the tool again.", nil
		},
	}
}

func renderTelegramTomatoGIF(message *telegram.Message, viewerID int64) ([]byte, error) {
	if message == nil {
		return nil, errors.New("target message is missing")
	}
	source, err := gif.DecodeAll(bytes.NewReader(tomatoThrowGIF))
	if err != nil {
		return nil, fmt.Errorf("decode tomato animation: %w", err)
	}
	if len(source.Image) == 0 {
		return nil, errors.New("tomato animation contains no frames")
	}
	faces, err := newTelegramFaces()
	if err != nil {
		return nil, err
	}

	base := renderTelegramMessage(message, viewerID, faces)
	output := &gif.GIF{
		Image:     make([]*image.Paletted, 0, len(source.Image)),
		Delay:     make([]int, 0, len(source.Image)),
		Disposal:  make([]byte, 0, len(source.Image)),
		LoopCount: source.LoopCount,
		Config: image.Config{
			ColorModel: tomatoPalette, Width: tomatoGIFWidth, Height: tomatoGIFHeight,
		},
	}

	sourceBounds := image.Rect(0, 0, source.Config.Width, source.Config.Height)
	sourceCanvas := image.NewRGBA(sourceBounds)
	var restore *image.RGBA
	for index, frame := range source.Image {
		if index > 0 {
			disposal := byte(gif.DisposalNone)
			if index-1 < len(source.Disposal) {
				disposal = source.Disposal[index-1]
			}
			applyGIFDisposal(sourceCanvas, source.Image[index-1].Bounds(), disposal, restore)
		}
		if index < len(source.Disposal) && source.Disposal[index] == gif.DisposalPrevious {
			restore = cloneRGBA(sourceCanvas)
		} else {
			restore = nil
		}
		draw.Draw(sourceCanvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		canvas := cloneRGBA(base)
		overlayTomatoFrame(canvas, sourceCanvas)
		paletted := image.NewPaletted(canvas.Bounds(), tomatoPalette)
		draw.Draw(paletted, paletted.Bounds(), canvas, image.Point{}, draw.Src)
		output.Image = append(output.Image, paletted)
		delay := 5
		if index < len(source.Delay) && source.Delay[index] > 0 {
			delay = source.Delay[index]
		}
		output.Delay = append(output.Delay, delay)
		output.Disposal = append(output.Disposal, gif.DisposalNone)
	}

	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, output); err != nil {
		return nil, fmt.Errorf("encode Telegram tomato animation: %w", err)
	}
	return encoded.Bytes(), nil
}

func renderTelegramMessage(message *telegram.Message, viewerID int64, faces telegramFaces) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, tomatoGIFWidth, tomatoGIFHeight))
	fill(canvas, canvas.Bounds(), color.RGBA{R: 14, G: 22, B: 33, A: 255})
	drawTelegramWallpaper(canvas)
	drawTelegramHeader(canvas, faces)
	drawTelegramComposer(canvas, faces)

	outgoing := message.From != nil && viewerID != 0 && message.From.ID == viewerID
	author := "Сообщение"
	if message.From != nil {
		author = message.From.DisplayName()
	}
	content := strings.TrimSpace(message.ContentText())
	mediaLabel := telegramMediaLabel(message)
	if content == "" && mediaLabel == "" {
		content = "Сообщение"
	}

	maximumTextWidth := 430
	lines := wrapTelegramText(content, faces.regular, maximumTextWidth, 7)
	textWidth := 0
	for _, line := range lines {
		textWidth = max(textWidth, font.MeasureString(faces.regular, line).Ceil())
	}
	mediaHeight := 0
	if mediaLabel != "" {
		mediaHeight = 122
		textWidth = max(textWidth, 310)
	}
	bubbleWidth := max(190, min(500, textWidth+46))
	lineHeight := 23
	bubbleHeight := 24 + len(lines)*lineHeight + 28 + mediaHeight
	if len(lines) == 0 {
		bubbleHeight -= 8
	}
	bubbleHeight = max(bubbleHeight, 80)

	bubbleY := tomatoHeader + (tomatoGIFHeight-tomatoHeader-tomatoComposer-bubbleHeight)/2
	bubbleX := 70
	if outgoing {
		bubbleX = tomatoGIFWidth - bubbleWidth - 54
	}
	bubble := image.Rect(bubbleX, bubbleY, bubbleX+bubbleWidth, bubbleY+bubbleHeight)
	bubbleColor := color.RGBA{R: 28, G: 43, B: 55, A: 255}
	if outgoing {
		bubbleColor = color.RGBA{R: 43, G: 82, B: 120, A: 255}
	}
	drawRoundedRect(canvas, bubble, 17, bubbleColor)
	drawBubbleTail(canvas, bubble, outgoing, bubbleColor)

	textX := bubble.Min.X + 16
	y := bubble.Min.Y + 21
	if !outgoing {
		drawText(canvas, faces.bold, textX, y, author, color.RGBA{R: 91, G: 174, B: 246, A: 255})
		y += 24
	}
	if mediaLabel != "" {
		mediaRect := image.Rect(textX, y-12, bubble.Max.X-16, y+mediaHeight-20)
		drawTelegramMedia(canvas, mediaRect, mediaLabel, faces)
		y += mediaHeight - 8
	}
	for _, line := range lines {
		drawText(canvas, faces.regular, textX, y, line, color.RGBA{R: 240, G: 244, B: 247, A: 255})
		y += lineHeight
	}

	stamp := time.Now().Format("15:04")
	if message.Date > 0 {
		stamp = time.Unix(message.Date, 0).Format("15:04")
	}
	stampWidth := font.MeasureString(faces.small, stamp).Ceil()
	stampX := bubble.Max.X - stampWidth - 15
	drawText(canvas, faces.small, stampX, bubble.Max.Y-10, stamp, color.RGBA{R: 142, G: 161, B: 177, A: 255})
	if outgoing {
		drawTelegramChecks(canvas, stampX-24, bubble.Max.Y-17)
	}
	return canvas
}

func drawTelegramHeader(canvas *image.RGBA, faces telegramFaces) {
	fill(canvas, image.Rect(0, 0, tomatoGIFWidth, tomatoHeader), color.RGBA{R: 23, G: 33, B: 43, A: 255})
	drawCircle(canvas, 34, 29, 18, color.RGBA{R: 49, G: 137, B: 206, A: 255})
	drawText(canvas, faces.header, 27, 36, "C", color.White)
	drawText(canvas, faces.bold, 64, 25, "Nyande chat", color.RGBA{R: 244, G: 247, B: 249, A: 255})
	drawText(canvas, faces.small, 64, 44, "участники чата", color.RGBA{R: 132, G: 158, B: 179, A: 255})
	drawSearchIcon(canvas, 646, 28)
	for _, y := range []int{21, 28, 35} {
		drawCircle(canvas, 690, y, 2, color.RGBA{R: 160, G: 178, B: 193, A: 255})
	}
}

func drawTelegramComposer(canvas *image.RGBA, faces telegramFaces) {
	top := tomatoGIFHeight - tomatoComposer
	fill(canvas, image.Rect(0, top, tomatoGIFWidth, tomatoGIFHeight), color.RGBA{R: 23, G: 33, B: 43, A: 255})
	drawPaperclip(canvas, 29, top+27)
	drawText(canvas, faces.regular, 58, top+34, "Сообщение", color.RGBA{R: 111, G: 130, B: 145, A: 255})
	drawCircle(canvas, 647, top+27, 17, color.RGBA{R: 43, G: 82, B: 120, A: 255})
	drawMicrophone(canvas, 647, top+26)
	drawCircle(canvas, 693, top+27, 17, color.RGBA{R: 48, G: 156, B: 215, A: 255})
	drawSendArrow(canvas, 693, top+27)
}

func drawTelegramWallpaper(canvas *image.RGBA) {
	ink := color.RGBA{R: 24, G: 36, B: 50, A: 255}
	for _, item := range [][3]int{{40, 95, 15}, {170, 105, 10}, {620, 105, 14}, {535, 205, 12}, {90, 360, 13}, {650, 355, 10}, {370, 390, 16}} {
		drawCircleOutline(canvas, item[0], item[1], item[2], ink)
	}
	for _, line := range [][4]int{{15, 200, 54, 180}, {205, 340, 245, 370}, {465, 92, 493, 120}, {575, 300, 618, 276}, {298, 85, 318, 112}} {
		drawLine(canvas, line[0], line[1], line[2], line[3], ink, 2)
	}
}

func drawTelegramMedia(canvas *image.RGBA, rectangle image.Rectangle, label string, faces telegramFaces) {
	drawRoundedRect(canvas, rectangle, 12, color.RGBA{R: 39, G: 57, B: 70, A: 255})
	for y := rectangle.Min.Y; y < rectangle.Max.Y; y++ {
		shade := uint8(45 + (y-rectangle.Min.Y)*24/max(1, rectangle.Dy()))
		fill(canvas, image.Rect(rectangle.Min.X, y, rectangle.Max.X, y+1), color.RGBA{R: shade, G: shade + 18, B: shade + 25, A: 255})
	}
	drawCircle(canvas, rectangle.Min.X+rectangle.Dx()/2, rectangle.Min.Y+rectangle.Dy()/2-8, 25, color.RGBA{R: 240, G: 244, B: 247, A: 220})
	icon := "▶"
	if label == "ФОТО" {
		icon = "●"
	} else if label == "АУДИО" || label == "ГОЛОСОВОЕ" {
		icon = "♪"
	} else if label == "ФАЙЛ" {
		icon = "▤"
	}
	iconWidth := font.MeasureString(faces.bold, icon).Ceil()
	drawText(canvas, faces.bold, rectangle.Min.X+(rectangle.Dx()-iconWidth)/2, rectangle.Min.Y+rectangle.Dy()/2, icon, color.RGBA{R: 43, G: 82, B: 120, A: 255})
	labelWidth := font.MeasureString(faces.small, label).Ceil()
	drawText(canvas, faces.small, rectangle.Min.X+(rectangle.Dx()-labelWidth)/2, rectangle.Max.Y-9, label, color.RGBA{R: 226, G: 234, B: 239, A: 255})
}

func overlayTomatoFrame(destination *image.RGBA, source *image.RGBA) {
	keyed := chromaKeyTomato(source)
	object := opaqueBounds(keyed)
	if object.Empty() {
		return
	}
	chatHeight := tomatoGIFHeight - tomatoHeader - tomatoComposer
	fullWidth := int(math.Round(float64(keyed.Bounds().Dx()) * float64(chatHeight) / float64(keyed.Bounds().Dy())))
	full := image.Rect((tomatoGIFWidth-fullWidth)/2, tomatoHeader, (tomatoGIFWidth+fullWidth)/2, tomatoGIFHeight-tomatoComposer)
	target := image.Rect(
		full.Min.X+object.Min.X*full.Dx()/keyed.Bounds().Dx(),
		full.Min.Y+object.Min.Y*full.Dy()/keyed.Bounds().Dy(),
		full.Min.X+object.Max.X*full.Dx()/keyed.Bounds().Dx(),
		full.Min.Y+object.Max.Y*full.Dy()/keyed.Bounds().Dy(),
	)
	target = expandRectangle(target, 1.65)
	scaleRGBA(destination, target, keyed.SubImage(object))
}

func opaqueBounds(source *image.RGBA) image.Rectangle {
	minimumX, minimumY := source.Bounds().Max.X, source.Bounds().Max.Y
	maximumX, maximumY := source.Bounds().Min.X, source.Bounds().Min.Y
	found := false
	for y := source.Bounds().Min.Y; y < source.Bounds().Max.Y; y++ {
		for x := source.Bounds().Min.X; x < source.Bounds().Max.X; x++ {
			if source.RGBAAt(x, y).A < 12 {
				continue
			}
			found = true
			minimumX, minimumY = min(minimumX, x), min(minimumY, y)
			maximumX, maximumY = max(maximumX, x+1), max(maximumY, y+1)
		}
	}
	if !found {
		return image.Rectangle{}
	}
	return image.Rect(minimumX, minimumY, maximumX, maximumY)
}

func expandRectangle(rectangle image.Rectangle, factor float64) image.Rectangle {
	centerX := float64(rectangle.Min.X+rectangle.Max.X) / 2
	centerY := float64(rectangle.Min.Y+rectangle.Max.Y) / 2
	halfWidth := float64(rectangle.Dx()) * factor / 2
	halfHeight := float64(rectangle.Dy()) * factor / 2
	return image.Rect(
		int(math.Round(centerX-halfWidth)), int(math.Round(centerY-halfHeight)),
		int(math.Round(centerX+halfWidth)), int(math.Round(centerY+halfHeight)),
	)
}

func chromaKeyTomato(source *image.RGBA) *image.RGBA {
	result := image.NewRGBA(source.Bounds())
	for y := source.Bounds().Min.Y; y < source.Bounds().Max.Y; y++ {
		for x := source.Bounds().Min.X; x < source.Bounds().Max.X; x++ {
			red, green, blue, alpha := source.At(x, y).RGBA()
			r, g, b, a := float64(red>>8), float64(green>>8), float64(blue>>8), uint8(alpha>>8)
			dominance := g - math.Max(r, b)
			if g > 80 && dominance > 25 {
				switch {
				case dominance >= 90:
					a = 0
				case dominance > 25:
					a = uint8(float64(a) * (90 - dominance) / 65)
				}
				g = math.Min(g, math.Max(r, b)*1.08)
			}
			result.SetRGBA(x, y, color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: a})
		}
	}
	return result
}

func scaleRGBA(destination *image.RGBA, target image.Rectangle, source image.Image) {
	if target.Empty() || source.Bounds().Empty() {
		return
	}
	for y := target.Min.Y; y < target.Max.Y; y++ {
		sourceY := source.Bounds().Min.Y + (y-target.Min.Y)*source.Bounds().Dy()/target.Dy()
		for x := target.Min.X; x < target.Max.X; x++ {
			if !image.Pt(x, y).In(destination.Bounds()) {
				continue
			}
			sourceX := source.Bounds().Min.X + (x-target.Min.X)*source.Bounds().Dx()/target.Dx()
			destination.Set(x, y, alphaOver(destination.At(x, y), source.At(sourceX, sourceY)))
		}
	}
}

func alphaOver(background, foreground color.Color) color.RGBA {
	back := color.NRGBAModel.Convert(background).(color.NRGBA)
	front := color.NRGBAModel.Convert(foreground).(color.NRGBA)
	alpha := float64(front.A) / 255
	return color.RGBA{
		R: uint8(float64(front.R)*alpha + float64(back.R)*(1-alpha)),
		G: uint8(float64(front.G)*alpha + float64(back.G)*(1-alpha)),
		B: uint8(float64(front.B)*alpha + float64(back.B)*(1-alpha)),
		A: 255,
	}
}

func telegramMediaLabel(message *telegram.Message) string {
	switch {
	case len(message.Photo) > 0:
		return "ФОТО"
	case message.Video != nil:
		return "ВИДЕО"
	case message.Animation != nil:
		return "GIF"
	case message.Audio != nil:
		return "АУДИО"
	case message.Voice != nil:
		return "ГОЛОСОВОЕ"
	case message.Document != nil:
		return "ФАЙЛ"
	default:
		return ""
	}
}

func wrapTelegramText(value string, face font.Face, maximumWidth, maximumLines int) []string {
	words := strings.Fields(value)
	lines := make([]string, 0, maximumLines)
	current := ""
	for len(words) > 0 && len(lines) < maximumLines {
		word := words[0]
		words = words[1:]
		candidate := word
		if current != "" {
			candidate = current + " " + word
		}
		if font.MeasureString(face, candidate).Ceil() <= maximumWidth {
			current = candidate
			continue
		}
		if current != "" {
			lines = append(lines, current)
			current = ""
			words = append([]string{word}, words...)
			continue
		}
		part, rest := splitTelegramWord(word, face, maximumWidth)
		lines = append(lines, part)
		if rest != "" {
			words = append([]string{rest}, words...)
		}
	}
	if current != "" && len(lines) < maximumLines {
		lines = append(lines, current)
	}
	if len(words) > 0 && len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		for last != "" && font.MeasureString(face, last+"…").Ceil() > maximumWidth {
			_, size := utf8.DecodeLastRuneInString(last)
			last = strings.TrimSpace(last[:len(last)-size])
		}
		lines[len(lines)-1] = last + "…"
	}
	return lines
}

func splitTelegramWord(word string, face font.Face, maximumWidth int) (string, string) {
	cut := 0
	for index := range word {
		if index == 0 {
			continue
		}
		if font.MeasureString(face, word[:index]).Ceil() > maximumWidth {
			break
		}
		cut = index
	}
	if cut == 0 {
		_, cut = utf8.DecodeRuneInString(word)
	}
	return word[:cut], word[cut:]
}

func newTelegramFaces() (telegramFaces, error) {
	regular, err := opentype.NewFace(tomatoRegularFont, &opentype.FaceOptions{Size: 18, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return telegramFaces{}, err
	}
	small, err := opentype.NewFace(tomatoRegularFont, &opentype.FaceOptions{Size: 13, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return telegramFaces{}, err
	}
	bold, err := opentype.NewFace(tomatoBoldFont, &opentype.FaceOptions{Size: 16, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return telegramFaces{}, err
	}
	header, err := opentype.NewFace(tomatoBoldFont, &opentype.FaceOptions{Size: 20, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return telegramFaces{}, err
	}
	return telegramFaces{regular: regular, small: small, bold: bold, header: header}, nil
}

func mustOpenTypeFont(data []byte) *opentype.Font {
	parsed, err := opentype.Parse(data)
	if err != nil {
		panic(err)
	}
	return parsed
}

func makeTelegramPalette() color.Palette {
	colors := color.Palette{
		color.RGBA{R: 14, G: 22, B: 33, A: 255}, color.RGBA{R: 23, G: 33, B: 43, A: 255},
		color.RGBA{R: 28, G: 43, B: 55, A: 255}, color.RGBA{R: 43, G: 82, B: 120, A: 255},
		color.RGBA{R: 49, G: 137, B: 206, A: 255}, color.RGBA{R: 48, G: 156, B: 215, A: 255},
		color.RGBA{R: 91, G: 174, B: 246, A: 255}, color.RGBA{R: 240, G: 244, B: 247, A: 255},
		color.RGBA{R: 142, G: 161, B: 177, A: 255}, color.RGBA{R: 111, G: 130, B: 145, A: 255},
		color.RGBA{R: 220, G: 53, B: 47, A: 255}, color.RGBA{R: 255, G: 99, B: 71, A: 255},
	}
	seen := make(map[color.RGBA]struct{}, 256)
	for _, item := range colors {
		seen[color.RGBAModel.Convert(item).(color.RGBA)] = struct{}{}
	}
	if animation, err := gif.DecodeAll(bytes.NewReader(tomatoThrowGIF)); err == nil {
		for _, frame := range animation.Image {
			for _, item := range frame.Palette {
				shade := color.RGBAModel.Convert(item).(color.RGBA)
				dominance := int(shade.G) - max(int(shade.R), int(shade.B))
				if shade.G > 80 && dominance > 25 {
					continue
				}
				if _, exists := seen[shade]; exists {
					continue
				}
				seen[shade] = struct{}{}
				colors = append(colors, shade)
				if len(colors) == 256 {
					return colors
				}
			}
		}
	}
	for red := 0; red < 6; red++ {
		for green := 0; green < 6; green++ {
			for blue := 0; blue < 6; blue++ {
				shade := color.RGBA{R: uint8(red * 51), G: uint8(green * 51), B: uint8(blue * 51), A: 255}
				if _, exists := seen[shade]; exists {
					continue
				}
				seen[shade] = struct{}{}
				colors = append(colors, shade)
				if len(colors) == 256 {
					return colors
				}
			}
		}
	}
	return colors
}

func applyGIFDisposal(canvas *image.RGBA, rectangle image.Rectangle, disposal byte, restore *image.RGBA) {
	switch disposal {
	case gif.DisposalBackground:
		draw.Draw(canvas, rectangle, image.Transparent, image.Point{}, draw.Src)
	case gif.DisposalPrevious:
		if restore != nil {
			draw.Draw(canvas, canvas.Bounds(), restore, image.Point{}, draw.Src)
		}
	}
}

func cloneRGBA(source *image.RGBA) *image.RGBA {
	copyImage := image.NewRGBA(source.Bounds())
	copy(copyImage.Pix, source.Pix)
	return copyImage
}

func drawText(destination draw.Image, face font.Face, x, baseline int, value string, shade color.Color) {
	drawer := font.Drawer{Dst: destination, Src: image.NewUniform(shade), Face: face, Dot: fixed.P(x, baseline)}
	drawer.DrawString(value)
}

func drawBubbleTail(destination *image.RGBA, bubble image.Rectangle, outgoing bool, shade color.RGBA) {
	x := bubble.Min.X - 5
	if outgoing {
		x = bubble.Max.X + 5
	}
	for radius := 7; radius > 0; radius-- {
		drawCircle(destination, x, bubble.Max.Y-radius, radius, shade)
		if outgoing {
			x++
		} else {
			x--
		}
	}
}

func drawCircleOutline(destination *image.RGBA, centerX, centerY, radius int, shade color.RGBA) {
	for angle := 0.0; angle < math.Pi*2; angle += 0.03 {
		x := centerX + int(math.Round(math.Cos(angle)*float64(radius)))
		y := centerY + int(math.Round(math.Sin(angle)*float64(radius)))
		if image.Pt(x, y).In(destination.Bounds()) {
			destination.SetRGBA(x, y, shade)
		}
	}
}

func drawLine(destination *image.RGBA, x0, y0, x1, y1 int, shade color.RGBA, width int) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		drawCircle(destination, x0, y0, max(1, width/2), shade)
		if x0 == x1 && y0 == y1 {
			break
		}
		twice := 2 * err
		if twice >= dy {
			err += dy
			x0 += sx
		}
		if twice <= dx {
			err += dx
			y0 += sy
		}
	}
}

func drawSearchIcon(destination *image.RGBA, x, y int) {
	shade := color.RGBA{R: 160, G: 178, B: 193, A: 255}
	drawCircleOutline(destination, x-3, y-3, 8, shade)
	drawLine(destination, x+3, y+3, x+10, y+10, shade, 2)
}

func drawPaperclip(destination *image.RGBA, x, y int) {
	shade := color.RGBA{R: 150, G: 169, B: 184, A: 255}
	drawCircleOutline(destination, x, y, 10, shade)
	drawLine(destination, x-5, y+5, x+6, y-6, shade, 2)
}

func drawMicrophone(destination *image.RGBA, x, y int) {
	shade := color.RGBA{R: 239, G: 245, B: 248, A: 255}
	drawRoundedRect(destination, image.Rect(x-4, y-10, x+5, y+6), 4, shade)
	drawLine(destination, x-8, y+2, x, y+10, shade, 2)
	drawLine(destination, x+8, y+2, x, y+10, shade, 2)
	drawLine(destination, x, y+10, x, y+14, shade, 2)
}

func drawSendArrow(destination *image.RGBA, x, y int) {
	shade := color.RGBA{R: 242, G: 247, B: 250, A: 255}
	drawLine(destination, x-9, y+7, x+10, y-8, shade, 3)
	drawLine(destination, x-9, y+7, x-5, y-4, shade, 3)
	drawLine(destination, x-5, y-4, x+10, y-8, shade, 3)
}

func drawTelegramChecks(destination *image.RGBA, x, y int) {
	shade := color.RGBA{R: 83, G: 186, B: 235, A: 255}
	drawLine(destination, x, y+3, x+5, y+8, shade, 2)
	drawLine(destination, x+5, y+8, x+13, y-1, shade, 2)
	drawLine(destination, x+7, y+5, x+11, y+8, shade, 2)
	drawLine(destination, x+11, y+8, x+19, y-1, shade, 2)
}
