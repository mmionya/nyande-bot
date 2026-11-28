package bot

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestSingleMediaAmbiguousDeliveryIsNotRepeated(t *testing.T) {
	for _, kind := range []string{"photo", "video"} {
		t.Run(kind, func(t *testing.T) {
			b := downloadedAnimationTestBot(t)
			var methods []string
			downloadedAnimationTransport(t, func(r *http.Request) (*http.Response, error) {
				methods = append(methods, filepath.Base(r.URL.Path))
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":"lost confirmation"}`))}, nil
			})
			item := downloader.Media{Kind: kind, Name: "media", Data: []byte("media bytes")}
			sent, err := b.sendDownloadedMedia(context.Background(), &telegram.Message{Chat: telegram.Chat{ID: 1}}, downloader.Result{Items: []downloader.Media{item}})
			method := "sendPhoto"
			if kind == "video" {
				method = "sendVideo"
			}
			if err == nil || len(sent) != 0 || !reflect.DeepEqual(methods, []string{method}) || len(b.media.items) != 0 {
				t.Fatalf("ambiguous upload retried or cached: methods=%v sent=%v err=%v", methods, sent, err)
			}
		})
	}
}
