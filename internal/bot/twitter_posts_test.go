package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/mmionya/nyande-bot/internal/access"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

type twitterPostsUnitCall struct {
	Method string
	Body   map[string]any
}

func newTwitterPostsUnitBot(t *testing.T, respond func(*http.Request, twitterPostsUnitCall) (int, string)) (*Bot, *[]twitterPostsUnitCall) {
	t.Helper()
	b := concurrentTestBot()
	var calls []twitterPostsUnitCall
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		call := twitterPostsUnitCall{Method: r.URL.Hostname() + r.URL.Path}
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&call.Body); err != nil {
				t.Error(err)
			}
		}
		calls = append(calls, call)
		code, body := respond(r, call)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return b, &calls
}

func twitterPostsUnitSuccess(id int) (int, string) {
	return http.StatusOK, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, id)
}

func TestTwitterPlainChunksKeepWhitespaceAndUTF16(t *testing.T) {
	value := "  **literal** *action*\n" + strings.Repeat("😀  \n", 1800) + "\n\t end  "
	chunks := splitTwitterPlainText(value, telegramMessageLimit)
	if len(chunks) < 2 || strings.Join(chunks, "") != value {
		t.Fatal("chunking changed source text")
	}
	for _, chunk := range chunks {
		if length := len(utf16.Encode([]rune(chunk))); length < 1 || length > telegramMessageLimit {
			t.Fatalf("invalid Telegram length: %d", length)
		}
	}
}

func TestTwitterTextPostSendsCompleteLiteralTextAndButtons(t *testing.T) {
	b, calls := newTwitterPostsUnitBot(t, func(r *http.Request, call twitterPostsUnitCall) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected request: %s", call.Method)
		}
		return twitterPostsUnitSuccess(100)
	})
	post := downloader.Post{Text: "\n**source** *action*  " + strings.Repeat("😺", 4000) + "\n\n", Author: "Name (@user)", URL: "https://x.com/user/status/123"}
	sent, err := b.sendTwitterTextPost(context.Background(), &telegram.Message{MessageID: 7, Chat: telegram.Chat{ID: 42}}, post)
	if err != nil || len(sent) != len(*calls) || len(sent) < 2 {
		t.Fatalf("sent=%d calls=%d err=%v", len(sent), len(*calls), err)
	}
	var joined strings.Builder
	for _, call := range *calls {
		text, _ := call.Body["text"].(string)
		joined.WriteString(text)
		if call.Body["chat_id"] != float64(42) || call.Body["parse_mode"] != nil || call.Body["entities"] != nil {
			t.Errorf("source scope or literal formatting changed: %#v", call.Body)
		}
		markup, _ := json.Marshal(call.Body["reply_markup"])
		if !strings.Contains(string(markup), `"callback_data":"twtranslate:123"`) {
			t.Errorf("missing button: %s", markup)
		}
	}
	if joined.String() != post.Text+"\n\n"+post.Author+"\n"+post.URL {
		t.Fatal("post content or footer changed")
	}
}

func TestTwitterTextPostRejectsEmptyAndReportsPartialDelivery(t *testing.T) {
	requests := 0
	b, _ := newTwitterPostsUnitBot(t, func(r *http.Request, call twitterPostsUnitCall) (int, string) {
		requests++
		if requests == 1 {
			return twitterPostsUnitSuccess(1)
		}
		return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"cannot send"}`
	})
	message := &telegram.Message{Chat: telegram.Chat{ID: 42}}
	if _, err := b.sendTwitterTextPost(context.Background(), message, downloader.Post{Text: " \n", Author: "Author", URL: "https://x.com/i/status/123"}); err == nil || requests != 0 {
		t.Fatal("empty post sent its footer as content")
	}
	sent, err := b.sendTwitterTextPost(context.Background(), message, downloader.Post{Text: strings.Repeat("x", 9000)})
	if err == nil || len(sent) != 1 || sent[0].MessageID != 1 || requests != 2 {
		t.Fatalf("partial send was retried or lost: sent=%v calls=%d err=%v", sent, requests, err)
	}
}

func TestTwitterTranslateReplyMarkupFetchesOnlyMetadata(t *testing.T) {
	metadata, model := 0, 0
	b, calls := newTwitterPostsUnitBot(t, func(r *http.Request, call twitterPostsUnitCall) (int, string) {
		switch r.URL.Hostname() {
		case "api.fxtwitter.com":
			metadata++
			if r.URL.Path != "/status/123" {
				t.Errorf("unexpected metadata endpoint: %s", r.URL.Path)
			}
			return 200, `{"code":200,"tweet":{"id":"123","text":"complete original text","url":"https://x.com/user/status/123","author":{"name":"Author","screen_name":"user"},"media":{"videos":[{"url":"https://video.twimg.com/do-not-download.mp4"}]}}}`
		case "llm.test":
			model++
			encoded, _ := json.Marshal(call.Body)
			if !strings.Contains(string(encoded), "complete original text") || strings.Contains(string(encoded), "truncated caption") {
				t.Errorf("wrong translation source: %s", encoded)
			}
			return 200, `{"choices":[{"message":{"content":"полный перевод"}}]}`
		case "api.telegram.org":
			return twitterPostsUnitSuccess(100)
		default:
			t.Errorf("translation downloaded media or another URL: %s", call.Method)
			return 500, `{}`
		}
	})
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 42}, From: &telegram.User{ID: 8}, ReplyToMessage: &telegram.Message{
		Caption:     "truncated caption",
		ReplyMarkup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{{Text: "Translate", CallbackData: "twtranslate:123"}}}},
	}}
	if err := b.translateCommand(context.Background(), message, "ru"); err != nil {
		t.Fatal(err)
	}
	if metadata != 1 || model != 1 {
		t.Fatalf("metadata=%d model=%d", metadata, model)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call.Method, "api.telegram.org/") && call.Body["chat_id"] != float64(42) {
			t.Errorf("translation left original chat: %#v", call.Body)
		}
	}
}

func TestTwitterTranslateReplyCaptionPreservesLiteralSource(t *testing.T) {
	const source = "  **literal** *action*\n    code < & >\n\n"
	var modelBody string
	b, calls := newTwitterPostsUnitBot(t, func(r *http.Request, call twitterPostsUnitCall) (int, string) {
		if r.URL.Hostname() == "llm.test" {
			encoded, _ := json.Marshal(call.Body)
			modelBody = string(encoded)
			return 200, `{"choices":[{"message":{"content":"**literal** *действие*\n    code"}}]}`
		}
		if r.URL.Hostname() != "api.telegram.org" {
			t.Errorf("unexpected source fetch: %s", call.Method)
		}
		return twitterPostsUnitSuccess(100)
	})
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 42}, From: &telegram.User{ID: 8}, ReplyToMessage: &telegram.Message{Caption: source}}
	if err := b.translateCommand(context.Background(), message, "kk"); err != nil {
		t.Fatal(err)
	}
	if modelBody == "" || !strings.Contains(modelBody, "literal") || !strings.Contains(modelBody, "code") {
		t.Fatalf("reply caption missing: %s", modelBody)
	}
	found := false
	for _, call := range *calls {
		if text, _ := call.Body["text"].(string); strings.Contains(text, "*действие*") {
			found = strings.Contains(text, "казахский") && call.Body["parse_mode"] == nil && call.Body["entities"] == nil
		}
	}
	if !found {
		t.Fatal("translation did not preserve literal formatting or target language")
	}
}

func TestTwitterTranslateMalformedDisabledAndCancelledNeverFetch(t *testing.T) {
	for _, test := range []string{"malformed", "long-id", "disabled", "silent", "banned", "cancelled"} {
		t.Run(test, func(t *testing.T) {
			b, _ := newTwitterPostsUnitBot(t, func(r *http.Request, call twitterPostsUnitCall) (int, string) {
				if r.URL.Hostname() != "api.telegram.org" {
					t.Errorf("forbidden translation fetched: %s", call.Method)
				}
				return twitterPostsUnitSuccess(100)
			})
			ctx := context.Background()
			callback := &telegram.CallbackQuery{ID: "cb", From: telegram.User{ID: 8}, Message: &telegram.Message{MessageID: 7, Chat: telegram.Chat{ID: 42}, From: &telegram.User{ID: 999, IsBot: true}}, Data: "twtranslate:123"}
			switch test {
			case "malformed":
				callback.Data = "twtranslate:https://127.0.0.1/private"
			case "long-id":
				callback.Data = twitterTranslatePrefix + strings.Repeat("1", 26)
			case "disabled":
				b.llmConfig = &llmChatSettings{disabledChats: map[int64]struct{}{42: {}}}
			case "silent":
				b.linkConfig = &linkDeletionSettings{silentChats: map[int64]struct{}{42: {}}}
			case "banned":
				var err error
				b.access, err = access.Open(filepath.Join(t.TempDir(), "bans.json"), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := b.access.SetBanned("8", true); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			}
			err := b.handleTwitterTranslationCallback(ctx, callback)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestTwitterTranslateCallbackUsesClickerAndAcknowledgesBeforeFetch(t *testing.T) {
	acknowledged, fetched, translated := false, false, false
	b, calls := newTwitterPostsUnitBot(t, func(r *http.Request, call twitterPostsUnitCall) (int, string) {
		switch r.URL.Hostname() {
		case "api.fxtwitter.com":
			if !acknowledged {
				t.Error("callback was not acknowledged before source request")
			}
			fetched = true
			return 200, `{"code":200,"tweet":{"id":"123","text":"Original","url":"https://x.com/user/status/123"}}`
		case "llm.test":
			translated = true
			return 200, `{"choices":[{"message":{"content":"Перевод"}}]}`
		case "api.telegram.org":
			if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
				acknowledged = true
				return 200, `{"ok":true,"result":true}`
			}
			return twitterPostsUnitSuccess(100)
		default:
			t.Errorf("unexpected request: %s", call.Method)
			return 500, `{}`
		}
	})
	callback := &telegram.CallbackQuery{ID: "cb", From: telegram.User{ID: 8}, Message: &telegram.Message{MessageID: 7, Chat: telegram.Chat{ID: 42}, From: &telegram.User{ID: 999, IsBot: true}}, Data: "twtranslate:123"}
	if err := b.handleTwitterTranslationCallback(context.Background(), callback); err != nil {
		t.Fatal(err)
	}
	if !acknowledged || !fetched || !translated {
		t.Fatalf("ack=%v fetched=%v translated=%v", acknowledged, fetched, translated)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call.Method, "api.telegram.org/") && !strings.HasSuffix(call.Method, "/answerCallbackQuery") && call.Body["chat_id"] != float64(42) {
			t.Errorf("callback output sent to another chat: %#v", call.Body)
		}
	}
}
