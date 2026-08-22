package downloader

import (
	"reflect"
	"testing"
)

func TestPinterestPinFromResponseAndImage(t *testing.T) {
	pin, err := pinterestPinFromResponse([]byte(`{
		"resource_response":{"data":{
			"id":"123", "title":"Cat room", "description":"A cozy room",
			"closeup_attribution":{"full_name":"Mimi"},
			"images":{
				"236x":{"url":"https://i.pinimg.com/236x/cat.jpg","width":236,"height":236},
				"orig":{"url":"https://i.pinimg.com/originals/cat.jpg","width":1200,"height":1600},
				"evil":{"url":"https://example.com/cat.jpg","width":9999,"height":9999}
			}
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	candidates := pinterestMediaCandidates(pin)
	want := []pinterestMediaCandidate{{kind: "photo", urls: []string{
		"https://i.pinimg.com/originals/cat.jpg",
		"https://i.pinimg.com/236x/cat.jpg",
	}}}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("unexpected Pinterest candidates:\nwant: %#v\n got: %#v", want, candidates)
	}
	if caption := pinterestCaption(pin); caption != "Cat room\n\nA cozy room\n\nMimi" {
		t.Fatalf("unexpected Pinterest caption: %q", caption)
	}
}

func TestPinterestCarouselPreservesMixedMediaOrder(t *testing.T) {
	pin := map[string]any{
		"carousel_data": map[string]any{"carousel_slots": []any{
			map[string]any{"images": map[string]any{
				"orig": map[string]any{"url": "https://i.pinimg.com/originals/one.jpg", "width": float64(1000), "height": float64(1500)},
			}},
			map[string]any{"videos": map[string]any{"video_list": map[string]any{
				"V_EXP7": map[string]any{"url": "https://v.pinimg.com/videos/small.mp4", "width": float64(360), "height": float64(640)},
				"V_HLS":  map[string]any{"url": "https://v.pinimg.com/videos/video.m3u8", "width": float64(1080), "height": float64(1920)},
				"V_720P": map[string]any{"url": "https://v.pinimg.com/videos/large.mp4", "width": float64(720), "height": float64(1280)},
			}}},
		}},
	}
	want := []pinterestMediaCandidate{
		{kind: "photo", urls: []string{"https://i.pinimg.com/originals/one.jpg"}},
		{kind: "video", urls: []string{
			"https://v.pinimg.com/videos/large.mp4",
			"https://v.pinimg.com/videos/small.mp4",
		}},
	}
	if got := pinterestMediaCandidates(pin); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected Pinterest carousel:\nwant: %#v\n got: %#v", want, got)
	}
}

func TestPinterestPinFromResponseRequiresData(t *testing.T) {
	if _, err := pinterestPinFromResponse([]byte(`{"resource_response":{"data":null}}`)); err == nil {
		t.Fatal("expected missing pin data to fail")
	}
}

func TestPinterestHLSVideoDoesNotFallBackToCoverImage(t *testing.T) {
	pin := map[string]any{
		"videos": map[string]any{"video_list": map[string]any{
			"V_HLS": map[string]any{"url": "https://v.pinimg.com/videos/video.m3u8"},
		}},
		"images": map[string]any{
			"orig": map[string]any{"url": "https://i.pinimg.com/originals/cover.jpg"},
		},
	}
	want := []pinterestMediaCandidate{{kind: "video"}}
	if got := pinterestMediaCandidates(pin); !reflect.DeepEqual(got, want) {
		t.Fatalf("HLS video cover must not be returned as the pin media: %#v", got)
	}
}
