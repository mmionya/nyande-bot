package downloader

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMediaCaptionLayout(t *testing.T) {
	for _, test := range []struct{ name, text, author, want string }{
		{"text and author", "Post text", "Name (@user)", "Post text\n\nName (@user)"},
		{"literal paragraphs", "  **Text**\r\n\r\n    code &lt;tag&gt;\n", "Name", "**Text**\n\n    code &lt;tag&gt;\n\nName"},
		{"author whitespace", "Post", " Name\n\t(@user) ", "Post\n\nName (@user)"},
		{"text only", " Post ", "", "Post"},
		{"author only", " \n", " Name ", "Name"},
		{"empty", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := formatMediaCaption(test.text, test.author); got != test.want {
				t.Fatalf("caption=%q, want %q", got, test.want)
			}
		})
	}
}

func TestMediaCaptionTruncationKeepsAttribution(t *testing.T) {
	const author = "Автор (@cat)"
	for _, text := range []string{strings.Repeat("я", 1500), strings.Repeat("😺", 1500), strings.Repeat("paragraph\n\n", 200)} {
		caption := formatMediaCaption(text, author)
		if !utf8.ValidString(caption) || utf8.RuneCountInString(caption) > mediaCaptionLimit || !strings.HasSuffix(caption, "…\n\n"+author) {
			t.Fatalf("long caption lost attribution or exceeded limit: %q", caption)
		}
		if truncateCaption(caption) != caption {
			t.Fatal("final caption truncation changed formatted attribution")
		}
	}
	caption := formatMediaCaption("Body", strings.Repeat("a", 2000))
	if utf8.RuneCountInString(caption) != mediaCaptionLimit || !strings.HasPrefix(caption, "…\n\n") || !strings.HasSuffix(caption, "…") {
		t.Fatalf("oversized author did not fit caption: %q", caption)
	}
}
