package bot

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

const statsCardSize = 1080

// Tonal surfaces and playful shapes inspired by metawka/expressive-profile.
// Render at 2x for smooth shapes, then downsample for Telegram. Fonts are embedded;
// rendering does not need network access, external processes, or installed fonts.
var statsColors = struct {
	background, surface, tile, high, primary, onPrimary, muted, text, peach color.RGBA
}{
	background: color.RGBA{23, 18, 22, 255},
	surface:    color.RGBA{35, 27, 32, 255},
	tile:       color.RGBA{49, 39, 45, 255},
	high:       color.RGBA{90, 61, 77, 255},
	primary:    color.RGBA{255, 179, 210, 255},
	onPrimary:  color.RGBA{83, 34, 58, 255},
	muted:      color.RGBA{210, 190, 200, 255},
	text:       color.RGBA{248, 233, 240, 255},
	peach:      color.RGBA{243, 188, 155, 255},
}

type statsPainter struct {
	canvas *image.RGBA
	faces  map[int]font.Face
	err    error
}

func renderStatsCard(stats statsSnapshot) ([]byte, error) {
	p := &statsPainter{
		canvas: image.NewRGBA(image.Rect(0, 0, statsCardSize*2, statsCardSize*2)),
		faces:  make(map[int]font.Face),
	}
	defer func() {
		for _, face := range p.faces {
			face.Close()
		}
	}()
	c := statsColors
	fill(p.canvas, p.canvas.Bounds(), c.background)
	p.rounded(24, 24, 1032, 1032, 48, c.surface)

	p.text(64, 83, 24, 600, "nyande", c.primary)
	p.text(61, 153, 60, 740, resources.Get("bot.stats.card.title"), c.text)
	p.text(64, 190, 23, 740, resources.Get("bot.stats.card.period"), c.muted)
	p.flower(923, 123, 69, 8, c.primary)
	p.text(886, 142, 47, 80, ":3", c.onPrimary)

	p.rounded(64, 216, 952, 68, 34, c.high)
	p.icon("clock", 98, 250, c.primary, c.high)
	p.text(126, 259, 23, 230, resources.Get("bot.stats.card.uptime"), c.text)
	p.rightText(987, 260, 28, 600, formatStatsUptime(stats.Uptime), c.text)

	p.metric(64, 308, "messages", resources.Get("bot.stats.card.messages"), stats.Messages, 8)
	p.metric(552, 308, "commands", resources.Get("bot.stats.card.commands"), stats.Commands, 4)
	p.metric(64, 474, "chats", resources.Get("bot.stats.card.chats"), stats.Chats, 6)
	p.metric(552, 474, "media", resources.Get("bot.stats.card.media"), stats.Media, 10)

	p.rounded(64, 640, 952, 190, 32, c.tile)
	p.text(92, 685, 27, 470, resources.Get("bot.stats.card.delivery"), c.text)
	rate := resources.Get("bot.stats.card.no_media")
	errorsCount := max(stats.MediaErrors, 0)
	if errorsCount > max(stats.Media, 0) {
		errorsCount = max(stats.Media, 0)
	}
	success := max(stats.Media-errorsCount, 0)
	errorRate := 0.0
	if stats.Media > 0 {
		errorRate = 100 * float64(errorsCount) / float64(stats.Media)
		rate = resources.Format("bot.stats.card.success_rate", map[string]any{"rate": statsPercent(100 - errorRate)})
	}
	p.rightText(988, 684, 23, 390, rate, c.primary)
	const barX, barY, barWidth, barHeight = 92, 712, 896, 18
	p.rounded(barX, barY, barWidth, barHeight, 9, c.high)
	if stats.Media > 0 {
		switch {
		case errorsCount == 0:
			p.rounded(barX, barY, barWidth, barHeight, 9, c.primary)
		case success == 0:
			p.rounded(barX, barY, barWidth, barHeight, 9, c.peach)
		default:
			// Keep a small nonzero segment visible, even when its share rounds to 0%.
			errorWidth := max(12, min(barWidth-18, int(math.Round(float64(barWidth-6)*errorRate/100))))
			successWidth := barWidth - 6 - errorWidth
			p.rounded(barX, barY, successWidth, barHeight, 9, c.primary)
			p.rounded(barX+successWidth+6, barY, errorWidth, barHeight, 9, c.peach)
		}
	}
	p.circle(98, 773, 6, c.primary)
	p.text(115, 782, 23, 400, resources.Format("bot.stats.card.success", map[string]any{"count": statsNumber(success)}), c.muted)
	p.circle(565, 773, 6, c.peach)
	p.text(582, 782, 23, 405, resources.Format("bot.stats.card.errors", map[string]any{
		"count": statsNumber(errorsCount), "rate": statsPercent(errorRate),
	}), c.muted)

	p.rounded(64, 850, 464, 124, 32, c.high)
	p.rounded(552, 850, 464, 124, 32, c.tile)
	p.text(92, 891, 23, 380, resources.Get("bot.stats.card.llm_calls"), c.text)
	p.text(92, 946, 43, 408, statsNumber(stats.LLMCalls), c.primary)
	p.text(580, 891, 23, 380, resources.Get("bot.stats.card.llm_errors"), c.muted)
	p.text(580, 946, 43, 408, statsNumber(stats.LLMErrors), c.peach)

	for x := 64; x < 282; x++ {
		y0 := 1015 + 5*math.Sin(float64(x-64)*math.Pi/26)
		y1 := 1015 + 5*math.Sin(float64(x+1-64)*math.Pi/26)
		drawLine(p.canvas, x*2, int(y0*2), (x+1)*2, int(y1*2), c.primary, 6)
	}
	p.rightText(1016, 1023, 20, 680, resources.Get("bot.stats.card.footer"), c.muted)
	if p.err != nil {
		return nil, p.err
	}
	card := image.NewRGBA(image.Rect(0, 0, statsCardSize, statsCardSize))
	// The exact 2:1 reduction only needs four samples per output pixel.
	// ApproxBiLinear avoids the large floating-point scratch buffer of BiLinear.
	draw.ApproxBiLinear.Scale(card, card.Bounds(), p.canvas, p.canvas.Bounds(), draw.Src, nil)
	var output bytes.Buffer
	if err := png.Encode(&output, card); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (p *statsPainter) metric(x, y int, icon, label string, value int64, petals int) {
	c := statsColors
	p.rounded(x, y, 464, 142, 30, c.tile)
	p.text(x+28, y+43, 25, 335, label, c.muted)
	p.flower(x+416, y+43, 25, petals, c.high)
	p.icon(icon, x+416, y+43, c.primary, c.high)
	p.text(x+26, y+113, 58, 408, statsNumber(value), c.text)
}

func (p *statsPainter) rounded(x, y, width, height, radius int, shade color.RGBA) {
	radius = min(radius, min(width, height)/2)
	drawRoundedRect(p.canvas, image.Rect(x*2, y*2, (x+width)*2, (y+height)*2), radius*2, shade)
}

func (p *statsPainter) circle(x, y, radius int, shade color.RGBA) {
	drawCircle(p.canvas, x*2, y*2, radius*2, shade)
}

func (p *statsPainter) flower(x, y, radius, petals int, shade color.RGBA) {
	r := radius * 2
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			angle := math.Atan2(float64(dy), float64(dx))
			edge := float64(r) * (0.89 + 0.11*math.Cos(float64(petals)*angle))
			if float64(dx*dx+dy*dy) <= edge*edge {
				p.canvas.SetRGBA(x*2+dx, y*2+dy, shade)
			}
		}
	}
}

func (p *statsPainter) icon(kind string, x, y int, ink, background color.RGBA) {
	line := func(x0, y0, x1, y1 int) {
		drawLine(p.canvas, (x+x0)*2, (y+y0)*2, (x+x1)*2, (y+y1)*2, ink, 4)
	}
	switch kind {
	case "clock":
		p.circle(x, y, 12, ink)
		p.circle(x, y, 10, background)
		line(0, -6, 0, 0)
		line(0, 0, 5, 3)
	case "messages":
		p.rounded(x-12, y-9, 24, 17, 5, ink)
		p.rounded(x-10, y-7, 20, 13, 3, background)
		line(-8, 7, -8, 12)
		line(-8, 12, -2, 7)
	case "commands":
		line(-9, -6, -3, 0)
		line(-3, 0, -9, 6)
		line(2, 7, 10, 7)
	case "chats":
		p.circle(x-4, y-5, 5, ink)
		p.circle(x+7, y-3, 4, ink)
		p.rounded(x-13, y+3, 18, 9, 4, ink)
		p.rounded(x+6, y+4, 8, 8, 4, ink)
	case "media":
		line(0, -10, 0, 4)
		line(-5, -1, 0, 4)
		line(0, 4, 5, -1)
		line(-10, 5, -10, 10)
		line(-10, 10, 10, 10)
		line(10, 10, 10, 5)
	}
}

func (p *statsPainter) face(size int) font.Face {
	if face, ok := p.faces[size]; ok {
		return face
	}
	face, err := opentype.NewFace(quoteFont, &opentype.FaceOptions{Size: float64(size * 2), DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		p.err = err
		return nil
	}
	p.faces[size] = face
	return face
}

func (p *statsPainter) fit(size, width int, value string) font.Face {
	for ; size >= 1; size-- {
		face := p.face(size)
		if face == nil || font.MeasureString(face, value).Ceil() <= width*2 {
			return face
		}
	}
	return nil
}

func (p *statsPainter) text(x, baseline, size, width int, value string, shade color.RGBA) {
	if face := p.fit(size, width, value); face != nil {
		drawText(p.canvas, face, x*2, baseline*2, value, shade)
	}
}

func (p *statsPainter) rightText(right, baseline, size, width int, value string, shade color.RGBA) {
	if face := p.fit(size, width, value); face != nil {
		drawText(p.canvas, face, right*2-font.MeasureString(face, value).Ceil(), baseline*2, value, shade)
	}
}

func statsNumber(value int64) string {
	digits := strconv.FormatInt(max(value, 0), 10)
	var result strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			result.WriteByte(' ')
		}
		result.WriteRune(digit)
	}
	return result.String()
}

func statsPercent(value float64) string {
	return strings.ReplaceAll(fmt.Sprintf("%.1f", value), ".", ",")
}

func formatStatsUptime(uptime time.Duration) string {
	seconds := int64(max(uptime, 0) / time.Second)
	days, hours, minutes := seconds/86400, seconds/3600%24, seconds/60%60
	fields := map[string]any{"days": days, "hours": hours, "minutes": minutes, "seconds": seconds % 60}
	switch {
	case days > 0:
		return resources.Format("bot.stats.card.uptime_days", fields)
	case hours > 0:
		return resources.Format("bot.stats.card.uptime_hours", fields)
	case minutes > 0:
		return resources.Format("bot.stats.card.uptime_minutes", fields)
	default:
		return resources.Format("bot.stats.card.uptime_seconds", fields)
	}
}
