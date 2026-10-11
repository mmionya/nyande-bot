package bot

import (
	"encoding/json"
	"strings"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func replyContext(message *telegram.Message) string {
	if message == nil || message.ReplyToMessage == nil {
		return ""
	}
	reply := message.ReplyToMessage
	text := []rune(strings.TrimSpace(reply.ContentText()))
	if len(text) == 0 {
		return ""
	}
	quoted := struct {
		Author    string `json:"author"`
		Text      string `json:"text"`
		Truncated bool   `json:"truncated,omitempty"`
	}{}
	if reply.SenderChat != nil {
		quoted.Author = strings.TrimSpace(reply.SenderChat.Title)
	} else if reply.From != nil {
		quoted.Author = strings.TrimSpace(reply.From.FirstName + " " + reply.From.LastName)
	}
	if quoted.Author == "" {
		quoted.Author = resources.Get("telegram.reply_context.unknown_author")
	}
	// ponytail: quote at most 4000 runes; add explicit retrieval if longer context is needed.
	if len(text) > 4000 {
		text, quoted.Truncated = text[:4000], true
	}
	quoted.Text = string(text)
	data, _ := json.Marshal(quoted) // Strings and a bool cannot fail to encode.
	return resources.Format("telegram.prompt.reply_context", map[string]any{"quoted": string(data)})
}
