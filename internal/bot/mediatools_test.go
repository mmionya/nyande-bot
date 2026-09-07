package bot

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestMediaFormattingHelpers(t *testing.T) {
	if got := parseFPS("30000/1001"); got < 29.9 || got > 30.0 {
		t.Errorf("parseFPS(30000/1001) = %f, want ~29.97", got)
	}
	if got := parseFPS("60/1"); got != 60 {
		t.Errorf("parseFPS(60/1) = %f, want 60", got)
	}
	if got := parseFPS("25"); got != 25 {
		t.Errorf("parseFPS(25) = %f, want 25", got)
	}

	if got := formatDuration(125); got != "02:05" {
		t.Errorf("formatDuration(125) = %s, want 02:05", got)
	}
	if got := formatDuration(3665); got != "01:01:05" {
		t.Errorf("formatDuration(3665) = %s, want 01:01:05", got)
	}

	if got := formatFileSize(500); got != "500 Б" {
		t.Errorf("formatFileSize(500) = %s", got)
	}
	if got := formatFileSize(1024 * 50); got != "50.0 КБ" {
		t.Errorf("formatFileSize = %s", got)
	}
	if got := formatFileSize(1024 * 1024 * 15); got != "15.00 МБ" {
		t.Errorf("formatFileSize = %s", got)
	}

	if got := formatAspect(1920, 1080); got != "16:9" {
		t.Errorf("formatAspect(1920, 1080) = %s, want 16:9", got)
	}
	if got := formatAspect(1080, 1080); got != "1:1" {
		t.Errorf("formatAspect(1080, 1080) = %s, want 1:1", got)
	}
}

func TestInspectPhoto(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	att := cachedAttachment{
		Kind: "photo",
		Name: "test.png",
		MIME: "image/png",
		Data: buf.Bytes(),
	}

	spec, err := inspectMedia(context.Background(), att)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec, "800x600") || !strings.Contains(spec, "PNG") || !strings.Contains(spec, "4:3") {
		t.Errorf("unexpected photo spec: %s", spec)
	}
}

func TestReminderTools(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test-reminders.db")
	store, err := reminders.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	b := &Bot{reminders: store}
	msg := &telegram.Message{
		Chat:      telegram.Chat{ID: -1001},
		MessageID: 55,
		From:      &telegram.User{ID: 123, FirstName: "Иван"},
	}

	tools := b.reminderTools(msg)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	reminderTool := tools[0]
	if reminderTool.Name != "set_reminder" {
		t.Errorf("expected tool set_reminder, got %s", reminderTool.Name)
	}

	// Test setting reminder
	res, err := reminderTool.Execute(ctx, map[string]string{
		"text":          "Купить торт",
		"delay_seconds": "30",
	})
	if err != nil {
		t.Fatalf("set_reminder failed: %v", err)
	}
	if !strings.Contains(res, "Купить торт") || !strings.Contains(res, "set successfully") {
		t.Errorf("unexpected output: %s", res)
	}

	// Verify in store
	due, err := store.GetDue(ctx, time.Now().Add(1*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Text != "Купить торт" || due[0].ChatID != -1001 {
		t.Errorf("unexpected due reminder in store: %+v", due)
	}

	// Test read_webpage tool with empty url
	webTool := tools[1]
	if webTool.Name != "read_webpage" {
		t.Errorf("expected tool read_webpage, got %s", webTool.Name)
	}
	resWeb, _ := webTool.Execute(ctx, map[string]string{"url": ""})
	if !strings.Contains(resWeb, "valid web page URL") {
		t.Errorf("unexpected output for empty URL: %s", resWeb)
	}
}
