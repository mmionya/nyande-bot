package downloader

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestInstagramPageMediaExtractsEscapedCarouselImages(t *testing.T) {
	page := `<meta content="A photo caption &amp; more" property="og:description">
		<script>
		display_url\":\"https:\\/\\/scontent.cdninstagram.com\\/one.jpg?x=1\\u0026y=2\"
		display_url\\\":\\\"https:\\\\/\\\\/scontent.cdninstagram.com\\\\/two.jpg\"
		display_url\":\"https:\\/\\/scontent.cdninstagram.com\\/one.jpg?x=1\\u0026y=2\"
		</script>`

	urls, caption := instagramPageMedia(page)
	expected := []string{
		"https://scontent.cdninstagram.com/one.jpg?x=1&y=2",
		"https://scontent.cdninstagram.com/two.jpg",
	}
	if !reflect.DeepEqual(urls, expected) {
		t.Fatalf("unexpected Instagram image URLs:\nwant: %#v\n got: %#v", expected, urls)
	}
	if caption != "A photo caption & more" {
		t.Fatalf("unexpected caption: %q", caption)
	}
}

func TestInstagramPageMediaPrefersVideoOverPreview(t *testing.T) {
	page := `<meta property="og:image" content="https://scontent.cdninstagram.com/preview.jpg">
		<script>{"video_url":"https:\/\/scontent.cdninstagram.com\/clip.mp4"}</script>`

	urls, _ := instagramPageMedia(page)
	expected := []string{"https://scontent.cdninstagram.com/clip.mp4"}
	if !reflect.DeepEqual(urls, expected) {
		t.Fatalf("expected video without preview image, got %#v", urls)
	}
}

func TestInstagramPageMediaExtractsPhotoCaptionFromJSON(t *testing.T) {
	page := `<script>{"display_url":"https:\/\/scontent.cdninstagram.com\/photo.jpg","caption_text":"Photo caption from Instagram"}</script>`

	urls, caption := instagramPageMedia(page)
	expected := []string{"https://scontent.cdninstagram.com/photo.jpg"}
	if !reflect.DeepEqual(urls, expected) {
		t.Fatalf("unexpected Instagram image URLs:\nwant: %#v\n got: %#v", expected, urls)
	}
	if caption != "Photo caption from Instagram" {
		t.Fatalf("unexpected Instagram photo caption: %q", caption)
	}
}

func TestCollectApifyMediaUsesResultEntriesAndSkipsCover(t *testing.T) {
	item := map[string]any{
		"thumb": "https://snapcdn.app/cover.jpg",
		"result": []any{
			map[string]any{"url": "https://snapcdn.app/photo-one.jpg"},
			map[string]any{"url": "https://snapcdn.app/photo-two.jpg"},
		},
	}
	expected := []string{
		"https://snapcdn.app/photo-one.jpg",
		"https://snapcdn.app/photo-two.jpg",
	}
	if actual := collectApifyMedia(item); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("unexpected Apify URLs:\nwant: %#v\n got: %#v", expected, actual)
	}
}

func TestInstagramApifyInputUsesActorURLField(t *testing.T) {
	data, err := json.Marshal(instagramApifyInput{
		URL: []string{"https://www.instagram.com/p/ABC123/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	if !strings.Contains(encoded, `"url"`) || strings.Contains(encoded, "directUrls") {
		t.Fatalf("unexpected Apify input: %s", encoded)
	}
}

func TestInstagramMediaHostsIncludeApifyProxy(t *testing.T) {
	for _, host := range []string{
		"scontent.cdninstagram.com",
		"instagram.fala.snapcdn.app",
		"api.apify.com",
	} {
		if !instagramMediaHost(host) {
			t.Errorf("expected trusted Instagram media host: %s", host)
		}
	}
	if instagramMediaHost("instagram.example.com") {
		t.Fatal("unrelated host must not be trusted")
	}
}
