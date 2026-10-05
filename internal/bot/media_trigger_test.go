package bot

import (
	"strings"
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

func TestGroupLLMQuoteRepliesWithoutMediaCache(t *testing.T) {
	application := &Bot{
		cfg:      config.Config{LLMTriggerWords: []string{"мяу"}},
		identity: telegram.User{ID: 99, Username: "nyande_bot"},
	}
	botUser := &telegram.User{ID: 99, IsBot: true}
	otherUser := &telegram.User{ID: 7}
	const caption = "Цитата #12 · /quote random"
	for _, test := range []struct {
		name, caption, text string
		from                *telegram.User
		photo, want         bool
	}{
		{"quote reply", caption, "ахах", botUser, true, false},
		{"quote reply with mention", caption, "@nyande_bot ахах", botUser, true, false},
		{"quote reply with trigger", caption, "Мяу, объясни", botUser, true, true},
		{"quote reply with partial trigger", caption, "мяукни", botUser, true, false},
		{"large quote ID", "Цитата #9223372036854775807 · /quote random", "ахах", botUser, true, false},
		{"ordinary bot photo", "Вот фото", "ахах", botUser, true, true},
		{"quote help text", caption, "ахах", botUser, false, true},
		{"caption from another user", caption, "@nyande_bot объясни", otherUser, true, true},
		{"missing sender", caption, "@nyande_bot объясни", nil, true, true},
		{"zero quote ID", "Цитата #0 · /quote random", "ахах", botUser, true, true},
		{"invalid quote ID", "Цитата #abc · /quote random", "ахах", botUser, true, true},
		{"incomplete caption", "Цитата #12", "ахах", botUser, true, true},
		{"extra caption text", caption + " extra", "ахах", botUser, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := &telegram.Message{MessageID: 10, From: test.from, Caption: test.caption}
			if test.photo {
				reply.Photo = []telegram.PhotoSize{{FileID: "quote-photo"}}
			}
			message := &telegram.Message{
				Chat: telegram.Chat{ID: -100123, Type: "supergroup"}, Text: test.text, ReplyToMessage: reply,
			}
			if strings.HasPrefix(test.text, "@nyande_bot") {
				message.Entities = []telegram.MessageEntity{{Type: "mention", Offset: 0, Length: 11}}
			}
			if got := application.shouldHandleGroupLLM(message); got != test.want {
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
