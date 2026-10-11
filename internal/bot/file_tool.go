package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

const (
	maxGeneratedFileBytes = 1 << 20
	maxGeneratedFiles     = 10
)

func (b *Bot) createFileTool(message *telegram.Message) llm.Tool {
	chatID, replyTo := message.Chat.ID, message.MessageID
	var mu sync.Mutex
	// ponytail: one attempt per filename per turn; use content hashes if in-turn revisions are needed.
	attempts := make(map[string]string)
	return llm.Tool{
		Name:        "create_file",
		Description: resources.Get("tool.create_file.description"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename": map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "description": resources.Get("tool.create_file.filename_description")},
				"content":  map[string]any{"type": "string", "minLength": 1, "maxLength": maxGeneratedFileBytes, "description": resources.Get("tool.create_file.content_description")},
			},
			"required": []string{"filename", "content"}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]string) (string, error) {
			name, content := strings.TrimSpace(args["filename"]), args["content"]
			if name == "" || name == "." || name == ".." || len(name) > 200 || !utf8.ValidString(name) ||
				strings.ContainsAny(name, `/\"`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
				return resources.Get("tool.create_file.invalid_filename"), nil
			}
			if content == "" || len(content) > maxGeneratedFileBytes || !utf8.ValidString(content) {
				return resources.Get("tool.create_file.invalid_content"), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if outcome, exists := attempts[name]; exists {
				return resources.Format("tool.create_file.already_attempted", map[string]any{"outcome": outcome}), nil
			}
			if len(attempts) >= maxGeneratedFiles {
				return resources.Get("tool.create_file.limit_reached"), nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			_, err := b.telegram.SendUpload(ctx, chatID, telegram.Upload{
				Kind: "document", Name: name, MIME: "text/plain; charset=utf-8", Data: []byte(content),
			}, replyTo)
			outcome := fmt.Sprintf(resources.Get("tool.create_file.sent"), name, len(content))
			if err != nil {
				log.Printf(resources.Get("telegram.log.file_delivery_failed"), chatID, replyTo, name, err)
				outcome = resources.Get("tool.create_file.delivery_unconfirmed")
			}
			attempts[name] = outcome
			return outcome, nil
		},
	}
}
