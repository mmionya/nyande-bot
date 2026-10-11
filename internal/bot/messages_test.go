package bot

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func TestWebpageLinksReachLLMWithinExistingChatRules(t *testing.T) {
	for _, test := range []struct {
		name, chatType, text, reply string
		enabled, moderated, ask     bool
	}{
		{"private summary", "private", "Перескажи https://example.com/article", "Summary", true, false, true},
		{"private URL", "private", "https://example.com/article", "Summary", true, false, true},
		{"addressed group", "supergroup", "нян, перескажи https://example.com/article", "Summary", true, false, true},
		{"unaddressed group", "supergroup", "Перескажи https://example.com/article", "", true, false, false},
		{"forbidden link", "supergroup", "нян, перескажи https://example.com/article", resources.Get("links.unsupported"), true, true, false},
		{"LLM disabled", "private", "Перескажи https://example.com/article", resources.Get("links.unsupported_private"), false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Config{LLMEnabled: test.enabled, LLMBaseURL: "https://llm.test", LLMModel: "test", LLMTimeout: time.Second, LLMTriggerWords: []string{"нян"}}
			b := &Bot{cfg: cfg, llm: llm.New(cfg), telegram: telegram.New("test"), state: NewState(), allowlist: &allowlist{}, adminCache: map[adminKey]adminEntry{
				{ChatID: 1, UserID: 2}: {expires: time.Now().Add(time.Hour)},
			}}
			if test.moderated {
				b.linkConfig = &linkDeletionSettings{enabledChats: map[int64]struct{}{1: {}}}
			}
			calls, deleted, reply := 0, false, ""
			typing := make(chan struct{})
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"ok":true,"result":true}`
				if r.URL.Hostname() == "llm.test" {
					calls++
					data, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(data), test.text) || !strings.Contains(string(data), `"read_webpage"`) {
						t.Error("webpage request lost its text or reading tool")
					}
					select {
					case <-typing:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					body = `{"choices":[{"message":{"content":"Summary"}}]}`
				} else if r.URL.Hostname() == "api.telegram.org" {
					switch {
					case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
						close(typing)
					case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
						deleted = true
					case strings.HasSuffix(r.URL.Path, "/sendMessage"):
						data, _ := io.ReadAll(r.Body)
						reply = string(data)
					default:
						t.Errorf("unexpected Telegram request: %s", r.URL.Path)
					}
				} else {
					t.Errorf("unexpected network request: %s", r.URL.Hostname())
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			message := &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 1, Type: test.chatType}, From: &telegram.User{ID: 2}, Text: test.text}
			if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			if (calls == 1) != test.ask || calls > 1 || deleted != test.moderated || (reply == "") != (test.reply == "") || !strings.Contains(reply, test.reply) {
				t.Fatalf("calls=%d deleted=%t reply=%q", calls, deleted, reply)
			}
		})
	}
}

func TestSplitTelegramMessagePreservesLongUnbrokenText(t *testing.T) {
	input := strings.Repeat("я", telegramMessageLimit*2+17)
	chunks := splitTelegramMessage(input, telegramMessageLimit)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	if strings.Join(chunks, "") != input {
		t.Fatal("split message lost content")
	}
	for _, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > telegramMessageLimit {
			t.Fatalf("chunk is too long: %d", utf8.RuneCountInString(chunk))
		}
	}
}

func TestSplitTelegramMessageCountsEmojiSafely(t *testing.T) {
	input := strings.Repeat("😺", 3000)
	chunks := splitTelegramMessage(input, telegramMessageLimit)
	if len(chunks) != 2 || strings.Join(chunks, "") != input {
		t.Fatalf("unexpected emoji split: chunks=%d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > telegramMessageLimit {
			t.Fatal("emoji chunk exceeds Telegram's UTF-16-safe limit")
		}
	}
}

func TestSplitTelegramMessagePrefersNaturalBoundary(t *testing.T) {
	input := strings.Repeat("word ", 30) + "\n\n" + strings.Repeat("tail ", 30)
	chunks := splitTelegramMessage(input, 180)
	if len(chunks) < 2 || !strings.HasSuffix(chunks[0], "word") {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
	if !strings.Contains(strings.Join(chunks, " "), "tail") {
		t.Fatal("tail content was lost")
	}
}

func TestSplitTelegramMessageRejectsEmptyInput(t *testing.T) {
	if chunks := splitTelegramMessage("   ", telegramMessageLimit); len(chunks) != 0 {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}
