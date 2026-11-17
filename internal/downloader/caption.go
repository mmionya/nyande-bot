package downloader

import "strings"

const mediaCaptionLimit = 1024

// Keep attribution in its own paragraph, including when the post text is too
// long for a media caption. Body whitespace and literal formatting stay intact.
func formatMediaCaption(text, author string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	author = strings.Join(strings.Fields(author), " ")
	if author == "" {
		return truncateCaption(text)
	}
	if text == "" {
		return truncateCaption(author)
	}
	// Leave room for the separator and at least an ellipsis for the body,
	// even if a provider sends an excessively long author name.
	author = truncateCaptionTo(author, mediaCaptionLimit-3)
	text = truncateCaptionTo(text, mediaCaptionLimit-2-len([]rune(author)))
	return text + "\n\n" + author
}

func truncateCaptionTo(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}
