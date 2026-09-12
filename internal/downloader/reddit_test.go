package downloader

import (
	"context"
	"net/url"
	"testing"
)

func TestResolveRedditGalleryURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://www.reddit.com/gallery/1w5nkpp", "https://www.reddit.com/comments/1w5nkpp/"},
		{"https://www.reddit.com/gallery/1w5nkpp/?share=1", "https://www.reddit.com/comments/1w5nkpp/?share=1"},
		{"https://old.reddit.com/gallery/1w5nkpp", "https://old.reddit.com/comments/1w5nkpp/"},
		{"https://www.reddit.com/r/cats/comments/abc/title/", "https://www.reddit.com/r/cats/comments/abc/title/"},
		{"https://www.reddit.com/gallery/", "https://www.reddit.com/gallery/"},
		{"https://reddit.com.example.org/gallery/1w5nkpp", "https://reddit.com.example.org/gallery/1w5nkpp"},
	} {
		t.Run(test.input, func(t *testing.T) {
			input, err := url.Parse(test.input)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := (&Downloader{}).resolveRedditURL(context.Background(), input)
			if err != nil || resolved.String() != test.want {
				t.Fatalf("resolved = %v, err = %v; want %s", resolved, err, test.want)
			}
			if input.String() != test.input {
				t.Fatal("resolution modified the input URL")
			}
		})
	}
}
