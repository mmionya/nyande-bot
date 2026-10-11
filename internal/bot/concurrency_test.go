package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestMediaDeliveryDoesNotWaitForLLM(t *testing.T) {
	for _, firstText := range []string{"Explain something", "Summarize https://example.com/article"} {
		t.Run(firstText, func(t *testing.T) {
			b := concurrentTestBot()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			started, release := make(chan struct{}), make(chan struct{})
			typing := make(chan struct{}, 3)
			answers := make(chan string, 3)
			var calls, videos atomic.Int32
			var workers sync.WaitGroup
			var releaseOnce sync.Once
			previous := http.DefaultTransport
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				cancel()
				workers.Wait()
				http.DefaultTransport = previous
			})
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				body, contentType := `{"ok":true,"result":{"message_id":100}}`, "application/json"
				switch r.URL.Hostname() {
				case "llm.test":
					var payload struct {
						Messages []struct{ Role, Content string }
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					select {
					case <-typing:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					if calls.Add(1) == 1 {
						close(started)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						body = `{"choices":[{"message":{"content":"First answer"}}]}`
					} else {
						user, assistant := false, false
						for _, message := range payload.Messages {
							user = user || (message.Role == "user" && strings.Contains(message.Content, firstText))
							assistant = assistant || (message.Role == "assistant" && message.Content == "First answer")
						}
						if !user || !assistant {
							t.Error("queued LLM request lost the preceding turn")
						}
						body = `{"choices":[{"message":{"content":"Second answer"}}]}`
					}
				case "8.8.8.8":
					body, contentType = "video bytes", "video/mp4"
				case "api.telegram.org":
					switch {
					case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
						typing <- struct{}{}
						body = `{"ok":true,"result":true}`
					case strings.HasSuffix(r.URL.Path, "/sendMessage"):
						var payload struct{ Text string }
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						if strings.HasSuffix(payload.Text, " answer") {
							answers <- payload.Text
						}
					case strings.HasSuffix(r.URL.Path, "/sendVideo"):
						data, err := io.ReadAll(r.Body)
						if err != nil || !strings.Contains(string(data), "video bytes") {
							t.Error("downloaded video was not included in the upload")
						}
						videos.Add(1)
					case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
						body = `{"ok":true,"result":true}`
					case strings.HasSuffix(r.URL.Path, "/editMessageText"):
					default:
						t.Errorf("unexpected Telegram request: %s", r.URL.Path)
					}
				default:
					t.Errorf("unexpected network request: %s", r.URL.Hostname())
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			run := func(id int, text string) <-chan error {
				done := make(chan error, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					done <- b.HandleUpdate(ctx, telegram.Update{Message: &telegram.Message{MessageID: id, Chat: telegram.Chat{ID: 1, Type: "private"}, From: &telegram.User{ID: 2}, Text: text}})
				}()
				return done
			}
			first := run(1, firstText)
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("LLM request did not start")
			}
			second := run(2, "Continue the explanation")
			media := run(3, "https://8.8.8.8/video.mp4")
			select {
			case err := <-media:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("media delivery waited for the blocked LLM request")
			}
			if calls.Load() != 1 || videos.Load() != 1 || len(answers) != 0 {
				t.Fatalf("while LLM blocked: calls=%d videos=%d answers=%d", calls.Load(), videos.Load(), len(answers))
			}
			releaseOnce.Do(func() { close(release) })
			for _, done := range []<-chan error{first, second} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("queued LLM request did not finish")
				}
			}
			if calls.Load() != 2 || len(answers) != 2 || <-answers != "First answer" || <-answers != "Second answer" {
				t.Fatal("LLM answers did not finish in conversation order")
			}
		})
	}
}

func TestSilentActivationWaitsForMediaDelivery(t *testing.T) {
	b := concurrentTestBot()
	settings, err := loadLinkDeletionSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.linkConfig = settings
	b.adminCache = map[adminKey]adminEntry{{ChatID: -1, UserID: 2}: {admin: true, expires: time.Now().Add(time.Hour)}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	var requests, videos atomic.Int32
	var workers sync.WaitGroup
	var releaseOnce sync.Once
	previous := http.DefaultTransport
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		workers.Wait()
		http.DefaultTransport = previous
	})
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body, contentType := `{"ok":true,"result":{"message_id":100}}`, "application/json"
		switch {
		case r.URL.Hostname() == "8.8.8.8":
			if requests.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			body, contentType = "video bytes", "video/mp4"
		case strings.HasSuffix(r.URL.Path, "/sendVideo"):
			if b.silentModeration(-1) {
				t.Error("media was sent after silent moderation activated")
			}
			videos.Add(1)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			body = `{"ok":true,"result":true}`
		case strings.HasSuffix(r.URL.Path, "/sendMessage"), strings.HasSuffix(r.URL.Path, "/editMessageText"):
		default:
			t.Errorf("unexpected network request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	update := func(id int, text string) telegram.Update {
		message := &telegram.Message{MessageID: id, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: 2}, Text: text}
		if text == silentModerationTrigger {
			message.Entities = []telegram.MessageEntity{{Type: "text_link", URL: "https://8.8.8.8/video.mp4", Length: len(text)}}
		}
		return telegram.Update{Message: message}
	}
	media, silent := make(chan error, 1), make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); media <- b.HandleUpdate(ctx, update(1, "https://8.8.8.8/video.mp4")) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("media request did not start")
	}
	workers.Add(1)
	go func() { defer workers.Done(); silent <- b.HandleUpdate(ctx, update(2, silentModerationTrigger)) }()
	select {
	case err := <-silent:
		t.Fatalf("silent moderation did not wait for media: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	for _, done := range []<-chan error{media, silent} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("media or silent activation did not finish")
		}
	}
	if !settings.Silent(-1) || videos.Load() != 1 {
		t.Fatal("media must finish before silent mode becomes active")
	}
	if err := b.HandleUpdate(ctx, update(3, "https://8.8.8.8/video.mp4")); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || videos.Load() != 1 {
		t.Fatal("silent chat still downloaded media")
	}
}

func TestDeliveredAlbumBatchIsAvailableWhileNextBatchSends(t *testing.T) {
	b := concurrentTestBot()
	b.identity.ID = 99
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	var releaseOnce sync.Once
	previous := http.DefaultTransport
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		workers.Wait()
		http.DefaultTransport = previous
	})
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		var result any
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMediaGroup"):
			messages := make([]telegram.Message, 10)
			for i := range messages {
				messages[i].MessageID = 100 + i
			}
			result = messages
		case strings.HasSuffix(r.URL.Path, "/sendVideo"):
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			result = telegram.Message{MessageID: 110}
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
		body, err := json.Marshal(map[string]any{"ok": true, "result": result})
		if err != nil {
			t.Error(err)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: 2}}
	result := downloader.Result{Items: make([]downloader.Media, 11)}
	for i := range result.Items {
		result.Items[i] = downloader.Media{Kind: "video", Name: "video.mp4", MIME: "video/mp4", Data: []byte("video bytes")}
	}
	done := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		sent, err := b.sendDownloadedMedia(ctx, message, result)
		if len(sent) != 11 {
			t.Errorf("sent %d items, want 11", len(sent))
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("second media batch did not start")
	}
	for id := 100; id < 110; id++ {
		if !b.media.Has(-1, id) {
			t.Fatalf("delivered message %d is unavailable until the entire album finishes", id)
		}
	}
	reply := &telegram.Message{Chat: message.Chat, Text: "nice video", ReplyToMessage: &telegram.Message{MessageID: 100, From: &telegram.User{ID: b.identity.ID}}}
	if b.shouldHandleGroupLLM(reply) {
		t.Fatal("reply to an already delivered video unexpectedly triggers the LLM")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("album did not finish")
	}
	if !b.media.Has(-1, 110) {
		t.Fatal("last delivered item is not cached")
	}
}

func TestLinkPermissionChangeWaitsForMedia(t *testing.T) {
	b, _ := permissionTestBot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	unlock := b.lockUpdates(-1, true)
	var release sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release.Do(unlock)
		cancel()
		workers.Wait()
	})
	done := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		done <- b.HandleUpdate(ctx, telegram.Update{Message: permissionMessage(1, 1, "/permitlinks 2")})
	}()
	select {
	case err := <-done:
		t.Fatalf("permission changed while media was still active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if b.linkConfig.UserPermitted(-1, 2) {
		t.Fatal("permission changed while media was still active")
	}
	release.Do(unlock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("permission command did not finish")
	}
	if !b.linkConfig.UserPermitted(-1, 2) {
		t.Fatal("permission was not granted after media finished")
	}
}

func concurrentTestBot() *Bot {
	cfg := config.Config{LLMEnabled: true, LLMBaseURL: "https://llm.test", LLMModel: "test", LLMTimeout: 5 * time.Second, MaxFileSize: 1024, MaxMediaItems: 10, RetryAttempts: 1}
	return &Bot{cfg: cfg, llm: llm.New(cfg), telegram: telegram.New("test"), downloader: downloader.New(cfg), state: NewState(), allowlist: &allowlist{}, media: newMediaCache(10)}
}
