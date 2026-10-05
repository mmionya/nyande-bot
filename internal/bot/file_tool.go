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
		Name: "create_file",
		Description: "Create a UTF-8 text file and send it as a document to the current Telegram chat. " +
			"When the user asks you to write code, a script, or a text file, write the complete content and call this tool unless they explicitly request only inline code or an explanation. " +
			"Any filename extension is allowed for UTF-8 text; there is no fixed list of supported languages or extensions. " +
			"Use the filename and extension requested by the user exactly; do not append .txt or substitute another extension. " +
			"Otherwise choose an appropriate name: Main.java for Java, main.cpp for C++, Program.cs for C#, Main.kt or script.kts for Kotlin, main.py, index.html, README.md, or data.json. " +
			"Provide raw file content, preserving indentation and newlines; do not wrap it in Markdown fences. " +
			"You may send up to 10 files of at most 1 MiB each per request, with a different filename for each file. " +
			"This tool creates the attachment from the supplied content; it does not read existing files or execute code. " +
			"Only create files requested by the user. Briefly acknowledge delivery after the tool confirms it; do not repeat the full code in your reply.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename": map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "description": "Complete filename including any user-requested extension, or no extension if requested. At most 200 UTF-8 bytes, without directories or control characters."},
				"content":  map[string]any{"type": "string", "minLength": 1, "maxLength": maxGeneratedFileBytes, "description": "Complete raw UTF-8 file content, at most 1 MiB. Preserve all indentation and newlines."},
			},
			"required": []string{"filename", "content"}, "additionalProperties": false,
		},
		Execute: func(ctx context.Context, args map[string]string) (string, error) {
			name, content := strings.TrimSpace(args["filename"]), args["content"]
			if name == "" || name == "." || name == ".." || len(name) > 200 || !utf8.ValidString(name) ||
				strings.ContainsAny(name, `/\"`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
				return "File not created: provide a plain filename of at most 200 UTF-8 bytes, without paths, quotes, or control characters.", nil
			}
			if content == "" || len(content) > maxGeneratedFileBytes || !utf8.ValidString(content) {
				return "File not created: content must be nonempty UTF-8 text of at most 1 MiB.", nil
			}
			mu.Lock()
			defer mu.Unlock()
			if outcome, exists := attempts[name]; exists {
				return "This filename was already attempted for this request; no file was sent again. Previous outcome: " + outcome, nil
			}
			if len(attempts) >= maxGeneratedFiles {
				return "File not created: the limit of 10 files for this request has been reached.", nil
			}
			if err := ctx.Err(); err != nil {
				return "", err
			}
			_, err := b.telegram.SendUpload(ctx, chatID, telegram.Upload{
				Kind: "document", Name: name, MIME: "text/plain; charset=utf-8", Data: []byte(content),
			}, replyTo)
			outcome := fmt.Sprintf("Successfully sent file %q (%d bytes) to the current chat in reply to the user's message. Briefly acknowledge delivery.", name, len(content))
			if err != nil {
				log.Printf("[file] delivery failed chat=%d reply=%d name=%q: %v", chatID, replyTo, name, err)
				outcome = "File delivery was not confirmed. Do not claim it was sent or retry this filename during this request."
			}
			attempts[name] = outcome
			return outcome, nil
		},
	}
}
