package bot

import (
	"testing"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestGroupLLMRequiresTriggerOnlyForDownloadedMediaReply(t *testing.T) {
	const chatID int64 = -100123
	application := &Bot{
		cfg:      config.Config{LLMTriggerWords: []string{"мяу", "нянде"}},
		identity: telegram.User{ID: 99, Username: "nyande_bot"},
		media:    newMediaCache(16),
	}
	application.media.Put(chatID,
		[]telegram.Message{{MessageID: 10}},
		[]cachedAttachment{{Kind: "photo", Name: "photo.jpg", Data: []byte{1}}},
	)
	botMessage := &telegram.Message{MessageID: 10, From: &telegram.User{ID: 99, IsBot: true}}
	normalBotMessage := &telegram.Message{MessageID: 11, From: &telegram.User{ID: 99, IsBot: true}}
	otherMessage := &telegram.Message{MessageID: 12, From: &telegram.User{ID: 7}}

	tests := []struct {
		name    string
		message telegram.Message
		want    bool
	}{
		{
			name: "downloaded media reply without trigger",
			message: telegram.Message{
				Chat: telegram.Chat{ID: chatID, Type: "group"}, Text: "что здесь?", ReplyToMessage: botMessage,
			},
			want: false,
		},
		{
			name: "downloaded media reply with trigger",
			message: telegram.Message{
				Chat: telegram.Chat{ID: chatID, Type: "group"}, Text: "мяу, что здесь?", ReplyToMessage: botMessage,
			},
			want: true,
		},
		{
			name: "trigger without mention or reply",
			message: telegram.Message{
				Chat: telegram.Chat{ID: chatID, Type: "group"}, Text: "Нянде, как дела?",
			},
			want: true,
		},
		{
			name: "ordinary bot reply without trigger",
			message: telegram.Message{
				Chat: telegram.Chat{ID: chatID, Type: "group"}, Text: "ответь мне", ReplyToMessage: normalBotMessage,
			},
			want: true,
		},
		{
			name: "mentioned bot while replying to another user",
			message: telegram.Message{
				Chat: telegram.Chat{ID: chatID, Type: "group"}, Text: "@nyande_bot закидай помидорами", ReplyToMessage: otherMessage,
				Entities: []telegram.MessageEntity{{Type: "mention", Offset: 0, Length: 11}},
			},
			want: true,
		},
		{
			name: "unaddressed group message",
			message: telegram.Message{
				Chat: telegram.Chat{ID: chatID, Type: "group"}, Text: "обычное сообщение",
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := application.shouldHandleGroupLLM(&test.message); got != test.want {
				t.Fatalf("shouldHandleGroupLLM() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestContainsTriggerWord(t *testing.T) {
	triggers := []string{"мяу", "нянде", "эй бот"}
	tests := []struct {
		text string
		want bool
	}{
		{text: "МЯУ, ответь", want: true},
		{text: "позови нянде!", want: true},
		{text: "эй бот — ты тут?", want: true},
		{text: "мяукни", want: false},
		{text: "няндевый", want: false},
		{text: "обычное сообщение", want: false},
	}
	for _, test := range tests {
		if got := containsTriggerWord(test.text, triggers); got != test.want {
			t.Errorf("containsTriggerWord(%q) = %t, want %t", test.text, got, test.want)
		}
	}
}

func TestMediaCacheHasDoesNotCopyAttachments(t *testing.T) {
	cache := newMediaCache(2)
	cache.Put(1, []telegram.Message{{MessageID: 2}}, []cachedAttachment{{Data: []byte{1, 2, 3}}})
	if !cache.Has(1, 2) {
		t.Fatal("cached media message was not recognized")
	}
	if cache.Has(1, 3) || cache.Has(2, 2) {
		t.Fatal("unrelated message was recognized as cached media")
	}
}
