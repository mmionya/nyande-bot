package downloader

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
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

func TestRedditImageURLsIgnoreVideoThumbnails(t *testing.T) {
	for _, test := range []struct {
		name string
		post string
		want []string
	}{
		{
			name: "native video",
			post: `{"is_video":true,"url":"https://v.redd.it/video","preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "external video",
			post: `{"post_hint":"rich:video","url":"https://youtu.be/video","preview":{"images":[{"source":{"url":"https://external-preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "crossposted video",
			post: `{"is_video":false,"url":"https://v.redd.it/video","crosspost_parent_list":[{"is_video":true}],"preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "animation",
			post: `{"post_hint":"image","url":"https://i.redd.it/animation.gif","preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
		},
		{
			name: "original image",
			post: `{"post_hint":"image","url_overridden_by_dest":"https://i.redd.it/original.png","preview":{"images":[{"source":{"url":"https://preview.redd.it/thumbnail.jpg"}}]}}`,
			want: []string{"https://i.redd.it/original.png"},
		},
		{
			name: "gallery preserves order",
			post: `{"gallery_data":{"items":[{"media_id":"b"},{"media_id":"a"}]},"media_metadata":{"a":{"s":{"u":"https://i.redd.it/a.png"}},"b":{"s":{"u":"https://preview.redd.it/b.jpg?width=100&amp;format=pjpg"}}}}`,
			want: []string{"https://preview.redd.it/b.jpg?width=100&format=pjpg", "https://i.redd.it/a.png"},
		},
		{
			name: "untrusted image host",
			post: `{"url":"https://i.redd.it.example.org/image.jpg"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var post map[string]any
			if err := json.Unmarshal([]byte(test.post), &post); err != nil {
				t.Fatal(err)
			}
			if got := redditImageURLs(post); !slices.Equal(got, test.want) {
				t.Fatalf("redditImageURLs() = %v; want %v", got, test.want)
			}
		})
	}
}
