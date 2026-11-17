package downloader

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSourceCaptionsUseConsistentAttribution(t *testing.T) {
	sources := []struct {
		name    string
		caption func(title, description, author string) string
	}{
		{"pinterest", func(title, description, author string) string {
			return pinterestCaption(map[string]any{
				"title": title, "description": description,
				"pinner": map[string]any{"full_name": author},
			})
		}},
		{"xiaohongshu", func(title, description, author string) string {
			return xiaohongshuCaption(map[string]any{
				"title": title, "desc": description,
				"user": map[string]any{"nickname": author},
			})
		}},
		{"yt-dlp", func(title, description, author string) string {
			return ytdlpCaptionFromInfo(map[string]any{
				"title": title, "description": description, "uploader": author,
			}, false)
		}},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			for _, test := range []struct {
				name, title, description, author, want string
			}{
				{"full", "Cats", "Two cats", "Alice", "Cats\n\nTwo cats\n\nAlice"},
				{"duplicate description", "Cats", "Cats", "Alice", "Cats\n\nAlice"},
				{"no author", "Cats", "Two cats", "", "Cats\n\nTwo cats"},
				{"author only", "", "", "Alice", "Alice"},
			} {
				t.Run(test.name, func(t *testing.T) {
					if got := source.caption(test.title, test.description, test.author); got != test.want {
						t.Fatalf("caption = %q, want %q", got, test.want)
					}
				})
			}
			t.Run("long text keeps author", func(t *testing.T) {
				caption := truncateCaption(source.caption("Cats", strings.Repeat("я", 1100), "Alice"))
				if !strings.HasPrefix(caption, "Cats\n\n") || !strings.HasSuffix(caption, "…\n\nAlice") {
					t.Fatalf("caption lost body layout or attribution: %q", caption)
				}
				if !utf8.ValidString(caption) || utf8.RuneCountInString(caption) > 1024 {
					t.Fatalf("invalid or oversized caption: %d runes", utf8.RuneCountInString(caption))
				}
			})
		})
	}
}
