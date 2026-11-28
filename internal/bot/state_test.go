package bot

import (
	"context"
	"testing"
	"time"

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

func TestMediaCacheBoundsBytesAndExpires(t *testing.T) {
	cache := newMediaCache(3)
	cache.maxBytes, cache.ttl = 6, time.Minute
	now := time.Now()
	cache.now = func() time.Time { return now }
	put := func(id int, data string) {
		cache.Put(1, []telegram.Message{{MessageID: id}}, []cachedAttachment{{Data: []byte(data)}})
	}
	put(1, "aaa")
	put(2, "bbb")
	if len(cache.Get(1, 1)) != 1 {
		t.Fatal("first file missing")
	}
	put(3, "ccc")
	if !cache.Has(1, 1) || cache.Has(1, 2) || !cache.Has(1, 3) {
		t.Fatal("byte limit must evict the least recently read attachment")
	}
	put(4, "oversized")
	if cache.Has(1, 4) || !cache.Has(1, 1) || !cache.Has(1, 3) {
		t.Fatal("oversized attachment must not displace usable cached files")
	}
	put(1, "a")
	put(5, "bb")
	if !cache.Has(1, 3) || !cache.Has(1, 5) || cache.bytes != 6 {
		t.Fatal("replacement did not release its previous byte budget")
	}
	now = now.Add(time.Minute)
	cache.prune()
	if cache.Has(1, 1) || len(cache.Get(1, 3)) != 0 || cache.bytes != 0 {
		t.Fatal("expired data must be released even without a new insertion")
	}
}
