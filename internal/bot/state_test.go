package bot

import (
	"context"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestAlbumReplySelectsItsOwnAttachment(t *testing.T) {
	for _, delivered := range []int{2, 1} {
		cache := newMediaCache(2)
		messages := []telegram.Message{{MessageID: 10}, {MessageID: 11}}
		attachments := []cachedAttachment{
			{Kind: "video", Name: "first.mp4", Data: []byte{1}},
			{Kind: "video", Name: "second.mp4", Data: []byte{2}},
		}
		cache.Put(1, messages[:delivered], attachments[:delivered])
		// The cache must own its bytes independently of the download result.
		for i := range attachments {
			attachments[i].Data[0] = 99
		}
		application := &Bot{media: cache}
		for i := delivered - 1; i >= 0; i-- {
			reply := &telegram.Message{Chat: telegram.Chat{ID: 1}, ReplyToMessage: &messages[i]}
			got, err := application.collectLLMAttachments(context.Background(), reply)
			if err != nil || len(got) != 1 || got[0].Name != attachments[i].Name || got[0].Data[0] != byte(i+1) {
				t.Fatalf("delivered=%d, reply=%d: got %#v, %v; want only %s", delivered, messages[i].MessageID, got, err, attachments[i].Name)
			}
			got[0].Data[0] = 88
			if again := cache.Get(1, messages[i].MessageID); again[0].Data[0] != byte(i+1) {
				t.Fatal("reading an attachment exposed the cached bytes to mutation")
			}
		}
		if delivered == 1 && cache.Has(1, messages[1].MessageID) {
			t.Fatal("failed album delivery cached an unsent attachment")
		}
		if cache.Has(2, messages[0].MessageID) {
			t.Fatal("album cache crossed chat boundaries")
		}
	}
}
