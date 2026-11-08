package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

type formattedAnswerEntity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url"`
	Language string `json:"language"`
}

type formattedAnswerRequest struct {
	ChatID          int64                   `json:"chat_id"`
	Text            string                  `json:"text"`
	Entities        []formattedAnswerEntity `json:"entities"`
	ParseMode       string                  `json:"parse_mode"`
	ReplyParameters *struct {
		MessageID int `json:"message_id"`
	} `json:"reply_parameters"`
}

func captureFormattedAnswer(t *testing.T, answer string) []formattedAnswerRequest {
	t.Helper()
	var requests []formattedAnswerRequest
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "api.telegram.org" || !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		var request formattedAnswerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":99}}`))}, nil
	})
	b := &Bot{telegram: telegram.New("test")}
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 42, Type: "private"}}
	if err := b.sendLLMAnswer(context.Background(), message, answer); err != nil {
		t.Fatal(err)
	}
	for index, request := range requests {
		if request.ChatID != 42 || request.ParseMode != "" {
			t.Fatalf("unexpected destination or text parser: %+v", request)
		}
		if index == 0 {
			if request.ReplyParameters == nil || request.ReplyParameters.MessageID != 17 {
				t.Fatal("first answer must reply to the user's message")
			}
		} else if request.ReplyParameters != nil {
			t.Fatal("continuations must not repeat the original reply")
		}
		units := utf16.Encode([]rune(request.Text))
		if len(units) == 0 || len(units) > telegramMessageLimit {
			t.Fatalf("invalid Telegram message length: %d", len(units))
		}
		for _, entity := range request.Entities {
			if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
				t.Fatalf("entity out of bounds: %+v", entity)
			}
		}
	}
	return requests
}

func formattedEntityText(t *testing.T, request formattedAnswerRequest, entity formattedAnswerEntity) string {
	t.Helper()
	units := utf16.Encode([]rune(request.Text))
	if entity.Offset < 0 || entity.Length <= 0 || entity.Offset+entity.Length > len(units) {
		t.Fatalf("entity out of bounds: %+v", entity)
	}
	return string(utf16.Decode(units[entity.Offset : entity.Offset+entity.Length]))
}

func TestLLMAnswerSendsCodeAndEmphasisAsTelegramEntities(t *testing.T) {
	code := "const selector = $('section.floor[data-floor=\"1\"] .guest[data-color=\"red\"]');\n" +
		"if (x < y && ready) console.log(\"**literal** _code_\");\n"
	answer := "😺 **жирный** и *действие* и _курсив_.\n\n```js\n" + code + "```\n\n" +
		"Проверь `.length` и [ссылку](https://example.com/a?x=1&y=2)."
	requests := captureFormattedAnswer(t, answer)
	if len(requests) != 1 {
		t.Fatalf("requests=%d, want 1", len(requests))
	}
	request := requests[0]
	if !strings.Contains(request.Text, code) || strings.Contains(request.Text, "```js") || strings.Contains(request.Text, "`.length`") || !strings.HasPrefix(request.Text, "😺 жирный и *действие* и курсив.") {
		t.Fatalf("Markdown markers or code content are incorrect: %q", request.Text)
	}
	want := map[string]string{"bold": "жирный", "italic": "курсив", "code": ".length", "pre": code, "text_link": "ссылку"}
	for _, entity := range request.Entities {
		expected, ok := want[entity.Type]
		if !ok {
			t.Fatalf("unexpected entity: %+v", entity)
		}
		if got := formattedEntityText(t, request, entity); got != expected {
			t.Fatalf("%s text=%q, want %q", entity.Type, got, expected)
		}
		if entity.Type == "pre" && entity.Language != "js" {
			t.Fatalf("code language=%q, want js", entity.Language)
		}
		if entity.Type == "text_link" && entity.URL != "https://example.com/a?x=1&y=2" {
			t.Fatalf("link destination changed: %q", entity.URL)
		}
		delete(want, entity.Type)
	}
	if len(want) > 0 {
		t.Fatalf("missing formatting entities: %v", want)
	}
}

func TestLLMAnswerSplitsLongCodeWithoutLosingIndentation(t *testing.T) {
	code := "  // leading indentation and trailing spaces must remain\n" +
		strings.Repeat("    console.log('😺 **literal** <tag> & value');  \n", 180) + "\n"
	requests := captureFormattedAnswer(t, "```js\n"+code+"```")
	if len(requests) < 2 {
		t.Fatalf("expected long code to be split, got %d messages", len(requests))
	}
	var combined strings.Builder
	for _, request := range requests {
		if len(request.Entities) != 1 || request.Entities[0].Type != "pre" || request.Entities[0].Language != "js" {
			t.Fatalf("each code continuation must retain its code formatting: %+v", request.Entities)
		}
		entity := request.Entities[0]
		if entity.Offset != 0 || formattedEntityText(t, request, entity) != request.Text {
			t.Fatal("code entity must span the entire code chunk")
		}
		combined.WriteString(request.Text)
	}
	if combined.String() != code {
		t.Fatal("splitting code lost or changed whitespace, emoji, or literal punctuation")
	}
}

func TestLLMAnswerSplitsBoldEmojiWithUTF16Offsets(t *testing.T) {
	text := strings.Repeat("😺", 2500)
	requests := captureFormattedAnswer(t, "**"+text+"**")
	if len(requests) != 2 {
		t.Fatalf("requests=%d, want 2", len(requests))
	}
	var combined strings.Builder
	for _, request := range requests {
		if len(request.Entities) != 1 || request.Entities[0].Type != "bold" || formattedEntityText(t, request, request.Entities[0]) != request.Text {
			t.Fatal("bold emoji formatting or UTF-16 entity boundaries were lost")
		}
		combined.WriteString(request.Text)
	}
	if combined.String() != text {
		t.Fatal("splitting bold emoji changed the answer")
	}
}

func TestLLMAnswerFormattingSurvivesFullReplyFlow(t *testing.T) {
	b := concurrentTestBot()
	typing := make(chan struct{}, 1)
	var sent formattedAnswerRequest
	answers := 0
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"ok":true,"result":true}`
		switch {
		case r.URL.Hostname() == "llm.test":
			select {
			case <-typing:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			body = `{"choices":[{"message":{"content":"**Жирный ответ** и *действие* и _курсив_."}}]}`
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			select {
			case typing <- struct{}{}:
			default:
			}
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			answers++
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			body = `{"ok":true,"result":{"message_id":99}}`
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	message := &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 42, Type: "private"}, From: &telegram.User{ID: 7}, Text: "Ответь с форматированием"}
	if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if answers != 1 || sent.Text != "Жирный ответ и *действие* и курсив." {
		t.Fatalf("answers=%d text=%q", answers, sent.Text)
	}
	styles := map[string]string{}
	for _, entity := range sent.Entities {
		styles[entity.Type] = formattedEntityText(t, sent, entity)
	}
	if styles["bold"] != "Жирный ответ" || styles["italic"] != "курсив" {
		t.Fatalf("formatting was changed between LLM response and Telegram: %#v", styles)
	}
}
