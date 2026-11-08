package bot

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
	"golang.org/x/net/dns/dnsmessage"
)

const twitterIntegrationID = "1234567890123456789"
const twitterIntegrationURL = "https://x.com/source/status/" + twitterIntegrationID

type twitterIntegrationRequest struct {
	Method          string
	ChatID          int64                          `json:"chat_id"`
	MessageID       int                            `json:"message_id"`
	Text            string                         `json:"text"`
	ParseMode       string                         `json:"parse_mode"`
	Entities        []telegram.MessageEntity       `json:"entities"`
	ReplyMarkup     *telegram.InlineKeyboardMarkup `json:"reply_markup"`
	CallbackQueryID string                         `json:"callback_query_id"`
	Upload          string
}

type twitterIntegrationNetwork struct {
	mu          sync.Mutex
	requests    []twitterIntegrationRequest
	postText    string
	media       bool
	failMarkup  bool
	fxCalls     int
	mediaCalls  int
	llmCalls    int
	llmRequests []string
}

func twitterIntegrationBot(t *testing.T, postText string) (*Bot, *twitterIntegrationNetwork, context.Context) {
	t.Helper()
	twitterIntegrationDNS(t)
	b := concurrentTestBot()
	b.identity = telegram.User{ID: 99, Username: "nyande_test_bot"}
	settings, err := loadLLMChatSettings(filepath.Join(t.TempDir(), "llm.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.llmConfig = settings
	network := &twitterIntegrationNetwork{postText: postText}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	previous := http.DefaultTransport
	nextID := 99
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		network.mu.Lock()
		defer network.mu.Unlock()
		body, mime, code := `{"ok":true,"result":true}`, "application/json", http.StatusOK
		switch r.URL.Hostname() {
		case "api.fxtwitter.com":
			network.fxCalls++
			if r.URL.Path != "/status/"+twitterIntegrationID {
				t.Errorf("unexpected metadata endpoint: %s", r.URL.Path)
			}
			tweet := map[string]any{"id": twitterIntegrationID, "text": network.postText,
				"author": map[string]string{"name": "Cat author", "screen_name": "cat"}}
			if network.media {
				tweet["media"] = map[string]any{"videos": []map[string]string{{"url": "https://video.twimg.com/mock.mp4"}}}
			}
			data, err := json.Marshal(map[string]any{"code": 200, "tweet": tweet})
			if err != nil {
				t.Error(err)
			}
			body = string(data)
		case "video.twimg.com":
			network.mediaCalls++
			body, mime = "mock twitter video bytes", "video/mp4"
		case "llm.test":
			network.llmCalls++
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			network.llmRequests = append(network.llmRequests, string(data))
			body = `{"choices":[{"message":{"content":"Переведённый *текст* с **буквальными** звёздочками."}}]}`
		case "api.telegram.org":
			request := twitterIntegrationRequest{Method: r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				request.Upload = string(data)
			} else if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			network.requests = append(network.requests, request)
			switch request.Method {
			case "sendMessage", "sendVideo", "sendDocument", "sendPhoto":
				nextID++
				body = fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, nextID)
			case "editMessageReplyMarkup":
				if network.failMarkup {
					code, body = http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"message cannot be edited"}`
				}
			case "editMessageText", "deleteMessage", "answerCallbackQuery", "sendChatAction":
			default:
				t.Errorf("unexpected Telegram method: %s", request.Method)
			}
		default:
			t.Errorf("unexpected external HTTP destination: %s", r.URL.Hostname())
			code = http.StatusBadGateway
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() {
		cancel()
		b.agentShutdown()
		http.DefaultTransport = previous
	})
	return b, network, ctx
}

func (n *twitterIntegrationNetwork) snapshot() []twitterIntegrationRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]twitterIntegrationRequest(nil), n.requests...)
}

func twitterIntegrationMessage(text string) *telegram.Message {
	return &telegram.Message{MessageID: 17, Chat: telegram.Chat{ID: 1, Type: "private"}, From: &telegram.User{ID: 10}, Text: text}
}

// The downloader validates public DNS before accepting media. Answer those
// lookups in memory as well, so the full upload test needs no external network.
func twitterIntegrationDNS(t *testing.T) {
	t.Helper()
	previous := net.DefaultResolver
	var workers sync.WaitGroup
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer server.Close()
			_ = server.SetDeadline(time.Now().Add(3 * time.Second))
			var size [2]byte
			if _, err := io.ReadFull(server, size[:]); err != nil {
				return
			}
			packet := make([]byte, binary.BigEndian.Uint16(size[:]))
			if _, err := io.ReadFull(server, packet); err != nil {
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(packet); err != nil {
				t.Error(err)
				return
			}
			reply := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions}
			for _, question := range query.Questions {
				if question.Type == dnsmessage.TypeA {
					reply.Answers = append(reply.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{8, 8, 8, 8}}})
				}
			}
			packet, err := reply.Pack()
			if err != nil {
				t.Error(err)
				return
			}
			binary.BigEndian.PutUint16(size[:], uint16(len(packet)))
			_, _ = server.Write(append(size[:], packet...))
		}()
		return client, nil
	}}
	t.Cleanup(func() { net.DefaultResolver = previous; workers.Wait() })
}

func TestTwitterIntegrationTextOnlyPreservesFullPostWithoutLLM(t *testing.T) {
	for _, source := range []string{
		"Text-only *post* with **literal markup**.\nSecond line.",
		strings.Repeat("🐈 *literal* **stars**  trailing spaces\n\n", 250) + "end",
	} {
		t.Run(fmt.Sprintf("length_%d", len(source)), func(t *testing.T) {
			b, network, ctx := twitterIntegrationBot(t, source)
			if err := b.HandleUpdate(ctx, telegram.Update{Message: twitterIntegrationMessage(twitterIntegrationURL)}); err != nil {
				t.Fatal(err)
			}
			var parts []string
			deleted := false
			for _, request := range network.snapshot() {
				if request.Method == "sendMessage" && request.Text != resources.Get("downloader.status.downloading") {
					parts = append(parts, request.Text)
					if request.ParseMode != "" || len(request.Entities) != 0 || len(utf16.Encode([]rune(request.Text))) > telegramMessageLimit {
						t.Fatalf("source formatting or chunk limit changed: %+v", request)
					}
					if request.ReplyMarkup == nil || twitterIDFromMarkup(request.ReplyMarkup) != twitterIntegrationID {
						t.Fatal("text post lacks a translation button")
					}
				}
				deleted = deleted || (request.Method == "deleteMessage" && request.MessageID == 100)
			}
			want := source + "\n\nCat author (@cat)\n" + twitterIntegrationURL
			if got := strings.Join(parts, ""); got != want {
				t.Fatalf("post text or attribution changed: got %q, want %q", got, want)
			}
			if !deleted || network.fxCalls != 1 || network.llmCalls != 0 || network.mediaCalls != 0 || b.state.MediaErrors.Load() != 0 {
				t.Fatalf("cleanup=%v metadata=%d llm=%d media=%d errors=%d", deleted, network.fxCalls, network.llmCalls, network.mediaCalls, b.state.MediaErrors.Load())
			}
		})
	}
}

func TestTwitterIntegrationButtonFailureDoesNotRepeatMedia(t *testing.T) {
	b, network, ctx := twitterIntegrationBot(t, "Video caption")
	network.media, network.failMarkup = true, true
	if err := b.HandleUpdate(ctx, telegram.Update{Message: twitterIntegrationMessage(twitterIntegrationURL)}); err != nil {
		t.Fatal(err)
	}
	uploads, buttons, cleanup := 0, 0, false
	for _, request := range network.snapshot() {
		switch request.Method {
		case "sendVideo", "sendDocument", "sendPhoto":
			uploads++
			if !strings.Contains(request.Upload, "mock twitter video bytes") {
				t.Fatal("downloaded media missing from upload")
			}
		case "editMessageReplyMarkup":
			buttons++
			if request.MessageID != 101 || twitterIDFromMarkup(request.ReplyMarkup) != twitterIntegrationID {
				t.Fatalf("button did not target the delivered video: %+v", request)
			}
		case "editMessageText":
			if request.Text != resources.Get("downloader.status.sending") {
				t.Fatalf("successful upload reported as a failure: %q", request.Text)
			}
		case "deleteMessage":
			cleanup = request.MessageID == 100
		}
	}
	if uploads != 1 || buttons != 1 || !cleanup || network.mediaCalls != 1 || network.fxCalls != 1 || network.llmCalls != 0 || b.state.MediaErrors.Load() != 0 {
		t.Fatalf("uploads=%d buttons=%d cleanup=%v metadata=%d media=%d llm=%d errors=%d", uploads, buttons, cleanup, network.fxCalls, network.mediaCalls, network.llmCalls, b.state.MediaErrors.Load())
	}
}

func TestTwitterIntegrationExplicitTranslationSkipsMediaDownload(t *testing.T) {
	for _, prefix := range []string{"/translate ", "/translate@nyande_test_bot ", "переведи "} {
		t.Run(prefix, func(t *testing.T) {
			b, network, ctx := twitterIntegrationBot(t, "Original full post *source* text.")
			network.media = true
			if err := b.HandleUpdate(ctx, telegram.Update{Message: twitterIntegrationMessage(prefix + twitterIntegrationURL)}); err != nil {
				t.Fatal(err)
			}
			translated, cleanup := false, false
			for _, request := range network.snapshot() {
				translated = translated || strings.Contains(request.Text, "Переведённый *текст* с **буквальными** звёздочками.")
				cleanup = cleanup || request.Method == "deleteMessage"
				if request.Method == "sendVideo" || request.Method == "sendPhoto" || request.Method == "sendDocument" || request.Text == resources.Get("downloader.status.downloading") {
					t.Fatal("translation used the automatic media path")
				}
			}
			if !translated || !cleanup || network.fxCalls != 1 || network.llmCalls != 1 || network.mediaCalls != 0 || !strings.Contains(strings.Join(network.llmRequests, ""), network.postText) {
				t.Fatalf("translation=%v cleanup=%v metadata=%d llm=%d media=%d", translated, cleanup, network.fxCalls, network.llmCalls, network.mediaCalls)
			}
		})
	}
}

func TestTwitterIntegrationOtherBotCommandIsIgnored(t *testing.T) {
	b, network, ctx := twitterIntegrationBot(t, "source")
	if err := b.HandleUpdate(ctx, telegram.Update{Message: twitterIntegrationMessage("/translate@other_bot " + twitterIntegrationURL)}); err != nil {
		t.Fatal(err)
	}
	if len(network.snapshot()) != 0 || network.fxCalls != 0 || network.llmCalls != 0 || network.mediaCalls != 0 {
		t.Fatal("command addressed to another bot was processed")
	}
}

func TestTwitterIntegrationBusyCallbackAcknowledgesWithoutQueue(t *testing.T) {
	b, network, ctx := twitterIntegrationBot(t, "source")
	unlock := b.lockUpdates(1, false)
	defer unlock()
	done := make(chan error, 1)
	go func() {
		done <- b.HandleUpdate(ctx, telegram.Update{CallbackQuery: &telegram.CallbackQuery{
			ID: "callback-1", From: telegram.User{ID: 10}, Message: twitterIntegrationMessage("source"), Data: twitterTranslatePrefix + twitterIntegrationID,
		}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("translation callback queued behind the conversation lock")
	}
	requests := network.snapshot()
	if len(requests) != 1 || requests[0].Method != "answerCallbackQuery" || requests[0].CallbackQueryID != "callback-1" || requests[0].Text != resources.Get("telegram.translate.busy") || network.llmCalls != 0 || network.fxCalls != 0 {
		t.Fatalf("busy callback response=%+v", requests)
	}
}

func TestTwitterIntegrationLLMOffKeepsPostsButDisablesTranslation(t *testing.T) {
	b, network, ctx := twitterIntegrationBot(t, "Text-only *post* stays available.")
	for _, text := range []string{"/llm off", "/translate " + twitterIntegrationURL, twitterIntegrationURL} {
		if err := b.HandleUpdate(ctx, telegram.Update{Message: twitterIntegrationMessage(text)}); err != nil {
			t.Fatal(err)
		}
	}
	postFound := false
	for _, request := range network.snapshot() {
		if strings.HasPrefix(request.Text, network.postText) {
			postFound = true
			if request.ReplyMarkup != nil {
				t.Fatal("disabled translation still advertised a button")
			}
		}
	}
	if !b.llmDisabled(1) || !postFound || network.fxCalls != 1 || network.llmCalls != 0 || network.mediaCalls != 0 {
		t.Fatalf("disabled=%v post=%v metadata=%d llm=%d media=%d", b.llmDisabled(1), postFound, network.fxCalls, network.llmCalls, network.mediaCalls)
	}
}
