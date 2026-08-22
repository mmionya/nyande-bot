package bot

import (
	"bytes"
	"image/gif"
	"image/png"
	"os"
	"testing"

	"github.com/hyphentae/nyande-bot/internal/telegram"
)

func TestRenderTelegramTomatoGIF(t *testing.T) {
	data, err := renderTelegramTomatoGIF(&telegram.Message{
		MessageID: 42,
		From:      &telegram.User{ID: 7, FirstName: "Кот", Username: "cat"},
		Text:      "Это сообщение сейчас закидают помидором!",
		Date:      1_723_568_400,
	}, 99)
	if err != nil {
		t.Fatal(err)
	}
	animation, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode generated GIF: %v", err)
	}
	source, err := gif.DecodeAll(bytes.NewReader(tomatoThrowGIF))
	if err != nil {
		t.Fatalf("decode source GIF: %v", err)
	}
	if len(animation.Image) != len(source.Image) || len(animation.Delay) != len(source.Delay) {
		t.Fatalf("unexpected animation length: frames=%d delays=%d", len(animation.Image), len(animation.Delay))
	}
	if animation.Config.Width != tomatoGIFWidth || animation.Config.Height != tomatoGIFHeight {
		t.Fatalf("unexpected dimensions: %dx%d", animation.Config.Width, animation.Config.Height)
	}
	if len(animation.Image) < 2 || bytes.Equal(animation.Image[0].Pix, animation.Image[len(animation.Image)/2].Pix) {
		t.Fatal("animation frames do not change")
	}
	if len(data) > 20*1024*1024 {
		t.Fatalf("generated GIF is unexpectedly large: %d bytes", len(data))
	}
	if preview := os.Getenv("TOMATO_GIF_PREVIEW"); preview != "" {
		if err := os.WriteFile(preview, data, 0o600); err != nil {
			t.Fatalf("write preview: %v", err)
		}
	}
	if preview := os.Getenv("TOMATO_FRAME_PREVIEW"); preview != "" {
		frame := animation.Image[len(animation.Image)/2]
		file, err := os.Create(preview)
		if err != nil {
			t.Fatalf("create frame preview: %v", err)
		}
		if err := png.Encode(file, frame); err != nil {
			_ = file.Close()
			t.Fatalf("encode frame preview: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close frame preview: %v", err)
		}
	}
}

func TestWrapTelegramTextSupportsCyrillic(t *testing.T) {
	faces, err := newTelegramFaces()
	if err != nil {
		t.Fatal(err)
	}
	lines := wrapTelegramText("Очень длинное сообщение на русском языке для пузыря Telegram", faces.regular, 180, 3)
	if len(lines) != 3 {
		t.Fatalf("unexpected lines: %#v", lines)
	}
	for _, line := range lines {
		if line == "" {
			t.Fatal("empty wrapped line")
		}
	}
}
