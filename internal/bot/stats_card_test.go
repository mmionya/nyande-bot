package bot

import (
	"bytes"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsCardRendersEmptyTypicalAndLargeCounters(t *testing.T) {
	for _, test := range []struct {
		name string
		data statsSnapshot
	}{
		{"empty", statsSnapshot{}},
		{"typical", statsSnapshot{Uptime: 199823 * time.Second, Chats: 24, Messages: 128406, Commands: 3812, Media: 5432, MediaErrors: 12, LLMCalls: 2401, LLMErrors: 3}},
		{"large", statsSnapshot{Uptime: time.Duration(math.MaxInt64), Chats: math.MaxInt64, Messages: math.MaxInt64, Commands: math.MaxInt64, Media: math.MaxInt64, MediaErrors: math.MaxInt64, LLMCalls: math.MaxInt64, LLMErrors: math.MaxInt64}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := renderStatsCard(test.data)
			if err != nil {
				t.Fatal(err)
			}
			card, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if card.Bounds().Dx() != statsCardSize || card.Bounds().Dy() != statsCardSize || len(data) > 10<<20 {
				t.Fatalf("invalid Telegram photo: bounds=%v bytes=%d", card.Bounds(), len(data))
			}
			if directory := os.Getenv("NYANDE_STATS_PREVIEW_DIR"); directory != "" {
				if err := os.WriteFile(filepath.Join(directory, "stats-"+test.name+".png"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestStatsCardFormatting(t *testing.T) {
	for _, test := range []struct {
		value int64
		want  string
	}{
		{-1, "0"}, {0, "0"}, {999, "999"}, {1000, "1 000"},
		{128406, "128 406"}, {math.MaxInt64, "9 223 372 036 854 775 807"},
	} {
		if got := statsNumber(test.value); got != test.want {
			t.Errorf("number %d: got %q, want %q", test.value, got, test.want)
		}
	}
	for _, test := range []struct {
		value time.Duration
		want  string
	}{
		{-time.Second, "0 с"}, {0, "0 с"}, {59 * time.Second, "59 с"},
		{61 * time.Second, "1 мин 1 с"}, {time.Hour, "1 ч 0 мин"},
		{25*time.Hour + 2*time.Minute, "1 д 1 ч 2 мин"},
	} {
		if got := formatStatsUptime(test.value); got != test.want {
			t.Errorf("uptime %v: got %q, want %q", test.value, got, test.want)
		}
	}
}
