package telegram

import (
	"encoding/json"
	"testing"
)

func TestStickerUpdateDecoding(t *testing.T) {
	var update Update
	err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":2,"chat":{"id":3,"type":"private"},"sticker":{"file_id":"original","file_unique_id":"unique","width":512,"height":256,"is_animated":true,"is_video":false,"emoji":"😂","thumbnail":{"file_id":"preview"},"file_size":123}}}`), &update)
	if err != nil || update.Message == nil || update.Message.Sticker == nil {
		t.Fatalf("sticker lost: %#v %v", update, err)
	}
	s := update.Message.Sticker
	if s.FileID != "original" || s.Thumbnail == nil || s.Thumbnail.FileID != "preview" || !s.IsAnimated || s.IsVideo || s.FileSize != 123 {
		t.Fatalf("incorrect sticker fields: %#v", s)
	}
}
