package logutil

import "testing"

func TestURLRemovesCredentialsAndSignedParameters(t *testing.T) {
	for input, want := range map[string]string{
		"https://user:secret@cdn.example.com/cat.mp4?token=secret#secret": "https://cdn.example.com/cat.mp4",
		"https://example.com/cat.jpg?":                                    "https://example.com/cat.jpg",
		"https://example.com/%0Acat.jpg":                                  "https://example.com/%0Acat.jpg",
		"not a URL containing secret":                                     "<invalid-url>",
		"https://[broken/secret":                                          "<invalid-url>",
	} {
		if got := URL(input); got != want {
			t.Errorf("URL()=%q; want %q", got, want)
		}
	}
}
