package downloader

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestXiaohongshuNoteFromPageExtractsPhotoCarousel(t *testing.T) {
	page := `<script>window.__INITIAL_STATE__ = {
		"note":{"noteDetailMap":{"abc123":{"note":{
			"title":"Cats", "desc":"Two cats", "user":{"nickname":"Mimi"},
			"imageList":[
				{"urlDefault":"https://sns-webpic-qc.xhscdn.com/one.webp","urlPre":"https://sns-webpic-qc.xhscdn.com/one-small.webp"},
				{"infoList":[{"imageScene":"WB_DFT","url":"https://sns-webpic-qc.xhscdn.com/two.jpg"}]}
			]
		}}}}, "unused":undefined, "literal":"undefined"};</script>`

	note, err := xiaohongshuNoteFromPage(page, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"https://sns-webpic-qc.xhscdn.com/one.webp", "https://sns-webpic-qc.xhscdn.com/one-small.webp"},
		{"https://sns-webpic-qc.xhscdn.com/two.jpg"},
	}
	if got := xiaohongshuImageURLs(note); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected image URLs:\nwant: %#v\n got: %#v", want, got)
	}
	if caption := xiaohongshuCaption(note); caption != "Cats\n\nTwo cats\n\nMimi" {
		t.Fatalf("unexpected caption: %q", caption)
	}
}

func TestXiaohongshuVideoURLsPreferOriginalAndRejectUntrustedHosts(t *testing.T) {
	note := map[string]any{
		"video": map[string]any{
			"consumer": map[string]any{"originVideoKey": "video/original.mp4"},
			"media": map[string]any{"stream": map[string]any{"h264": []any{
				map[string]any{
					"width": float64(1080), "height": float64(1920),
					"masterUrl":  "https://sns-video-hw.xhscdn.com/large.mp4",
					"backupUrls": []any{"https://evil.example/video.mp4"},
				},
			}}},
		},
	}
	want := []string{
		"https://sns-video-bd.xhscdn.com/video/original.mp4",
		"https://sns-video-hw.xhscdn.com/large.mp4",
	}
	if got := xiaohongshuVideoURLs(note); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected video URLs:\nwant: %#v\n got: %#v", want, got)
	}
}

func TestXiaohongshuVideoURLsDoNotTreatCoverAsMedia(t *testing.T) {
	note := map[string]any{
		"video": map[string]any{"media": map[string]any{}},
		"imageList": []any{
			map[string]any{"urlDefault": "https://sns-webpic-qc.xhscdn.com/cover.webp"},
		},
	}
	if got := xiaohongshuVideoURLs(note); len(got) != 0 {
		t.Fatalf("expected no video URLs, got %#v", got)
	}
}

func TestReplaceJavaScriptUndefinedLeavesStringsUntouched(t *testing.T) {
	input := `{"missing":undefined,"text":"undefined","identifier":someundefined}`
	want := `{"missing":null,"text":"undefined","identifier":someundefined}`
	if got := replaceJavaScriptUndefined(input); got != want {
		t.Fatalf("unexpected JavaScript conversion: %s", got)
	}
}

func TestXiaohongshuInitialStateRequiresCompleteObject(t *testing.T) {
	_, err := extractAssignedJSONObject(`window.__INITIAL_STATE__={"note":{}`, "window.__INITIAL_STATE__")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected incomplete-object error, got %v", err)
	}
}

func TestRecoveredXiaohongshuPostURLUsesNoteIDFromErrorRedirect(t *testing.T) {
	redirected, err := url.Parse("https://www.xiaohongshu.com/404?noteId=6956513d000000001e022eda&errorCode=-510001&xsec_token=token")
	if err != nil {
		t.Fatal(err)
	}
	recovered := recoveredXiaohongshuPostURL(redirected)
	if recovered == nil {
		t.Fatal("expected a recovered post URL")
	}
	if recovered.Path != "/explore/6956513d000000001e022eda" {
		t.Fatalf("unexpected recovered path: %s", recovered.Path)
	}
	if recovered.Query().Get("xsec_token") != "token" || recovered.Query().Has("errorCode") || recovered.Query().Has("noteId") {
		t.Fatalf("unexpected recovered query: %s", recovered.RawQuery)
	}
}
