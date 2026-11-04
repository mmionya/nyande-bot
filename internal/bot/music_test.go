package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/music"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

type musicRequest struct {
	method  string
	text    string
	payload map[string]any
}

func musicTestBot(t *testing.T) (*Bot, *[]musicRequest) {
	t.Helper()
	b := concurrentTestBot()
	b.identity.Username = "nyande_bot"
	requests := []musicRequest{}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	nextID := 100
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.telegram.org" {
			t.Fatalf("unexpected request %s", r.URL.Host)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		request := musicRequest{method: method}
		if method == "sendAudio" {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			defer r.MultipartForm.RemoveAll()
			request.payload = map[string]any{"title": r.FormValue("title"), "performer": r.FormValue("performer"), "duration": r.FormValue("duration")}
			file, _, err := r.FormFile("audio")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, _ := io.ReadAll(file)
			if string(data) != "MP3 test bytes" {
				t.Fatalf("wrong audio payload %q", data)
			}
		} else {
			if err := json.NewDecoder(r.Body).Decode(&request.payload); err != nil {
				t.Fatal(err)
			}
			request.text, _ = request.payload["text"].(string)
		}
		requests = append(requests, request)
		nextID++
		body := fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, nextID)
		if method == "answerCallbackQuery" || method == "deleteMessage" || method == "sendChatAction" {
			body = `{"ok":true,"result":true}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	b.music = &telegramMusic{sessions: map[string]telegramMusicSearch{}, search: func(_ context.Context, query string, limit int) ([]music.Track, error) {
		tracks := make([]music.Track, min(limit, 7))
		for i := range tracks {
			tracks[i] = music.Track{ID: fmt.Sprint(i), Title: fmt.Sprintf("%s #%d", query, i+1), Author: "Artist", URL: "https://youtu.be/song", Duration: 123 * time.Second}
		}
		return tracks, nil
	}, download: func(context.Context, music.Track) (downloader.Media, error) {
		return downloader.Media{Kind: "audio", Name: "song.mp3", MIME: "audio/mpeg", Data: []byte("MP3 test bytes")}, nil
	}}
	return b, &requests
}

func musicMessage(text string) telegram.Update {
	return telegram.Update{Message: &telegram.Message{MessageID: 10, From: &telegram.User{ID: 2}, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: text}}
}
func musicCallback(s telegramMusicSearch, action, value string) telegram.Update {
	return telegram.Update{CallbackQuery: &telegram.CallbackQuery{ID: "callback", From: telegram.User{ID: s.userID}, Message: &telegram.Message{MessageID: s.messageID, Chat: telegram.Chat{ID: s.chatID, Type: "private"}}, Data: "music:" + s.id + ":" + action + ":" + value}}
}

func TestTelegramMusicSearchSelectAndPlay(t *testing.T) {
	b, requests := musicTestBot(t)
	b.cfg.LLMEnabled = false
	b.llm = nil // Music discovery and sending must not require an LLM.
	ctx := context.Background()
	if err := b.HandleUpdate(ctx, musicMessage("/search@nyande_bot sc: song")); err != nil {
		t.Fatal(err)
	}
	s, ok := b.music.latest(1, 2)
	if !ok || len(s.tracks) != 7 || s.source != "sc" {
		t.Fatalf("missing search: %+v", s)
	}
	last := (*requests)[len(*requests)-1]
	if last.method != "editMessageText" || !strings.Contains(last.text, "Artist · 2:03") || !strings.Contains(last.text, "1/2") {
		t.Fatalf("bad results: %+v", last)
	}
	encoded, _ := json.Marshal(last.payload["reply_markup"])
	var keyboard struct {
		Rows [][]struct {
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(encoded, &keyboard); err != nil {
		t.Fatal(err)
	}
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if len(button.Data) > 64 {
				t.Fatal("callback exceeds Telegram limit")
			}
		}
	}
	if err := b.HandleUpdate(ctx, musicCallback(s, "page", "1")); err != nil {
		t.Fatal(err)
	}
	last = (*requests)[len(*requests)-1]
	if !strings.Contains(last.text, "6. sc: song #6") || strings.Contains(last.text, "1. sc: song #1") {
		t.Fatal("pagination did not change visible tracks")
	}
	for _, update := range []telegram.Update{musicCallback(s, "get", "5"), musicMessage("/play 2"), musicMessage("/play ytm: another")} {
		before := len(*requests)
		if err := b.HandleUpdate(ctx, update); err != nil {
			t.Fatal(err)
		}
		audios := 0
		for _, request := range (*requests)[before:] {
			if request.method == "sendAudio" {
				audios++
				if request.payload["performer"] != "Artist" || request.payload["duration"] != "123" {
					t.Fatal("audio metadata missing")
				}
			}
		}
		if audios != 1 {
			t.Fatalf("sent %d audio messages", audios)
		}
	}
}

func TestTelegramMusicBusyChatDoesNotLeaveCallbacksWaiting(t *testing.T) {
	b, requests := musicTestBot(t)
	if err := b.HandleUpdate(context.Background(), musicMessage("/search song")); err != nil {
		t.Fatal(err)
	}
	s, _ := b.music.latest(1, 2)
	unlock := b.lockUpdates(1, true)
	done := make(chan error, 1)
	go func() { done <- b.HandleUpdate(context.Background(), musicCallback(s, "get", "0")) }()
	select {
	case err := <-done:
		unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		unlock()
		<-done
		t.Fatal("callback waited behind a download instead of reporting busy")
	}
	last := (*requests)[len(*requests)-1]
	if last.method != "answerCallbackQuery" || last.text != resources.Get("telegram.music.busy") {
		t.Fatal("missing immediate callback acknowledgement")
	}
}

func TestTelegramMusicCallbacksAreScopedAndExpire(t *testing.T) {
	for _, mode := range []string{"user", "chat", "message", "expired", "negative", "overflow", "malformed", "unknown source"} {
		t.Run(mode, func(t *testing.T) {
			b, requests := musicTestBot(t)
			if err := b.HandleUpdate(context.Background(), musicMessage("/music song")); err != nil {
				t.Fatal(err)
			}
			s, _ := b.music.latest(1, 2)
			u := musicCallback(s, "get", "0")
			switch mode {
			case "user":
				u.CallbackQuery.From.ID = 99
			case "chat":
				u.CallbackQuery.Message.Chat.ID = 99
			case "message":
				u.CallbackQuery.Message.MessageID++
			case "expired":
				s.expires = time.Now().Add(-time.Second)
				b.music.sessions[s.id] = s
			case "negative":
				u = musicCallback(s, "get", "-1")
			case "overflow":
				u = musicCallback(s, "get", "999")
			case "malformed":
				u.CallbackQuery.Data = "music:broken"
			case "unknown source":
				u = musicCallback(s, "source", "local")
			}
			before := len(*requests)
			if err := b.HandleUpdate(context.Background(), u); err != nil {
				t.Fatal(err)
			}
			if len(*requests) != before+1 || (*requests)[before].method != "answerCallbackQuery" || (*requests)[before].text != resources.Get("telegram.music.expired") {
				t.Fatal("invalid callback was not rejected")
			}
		})
	}
}

func TestTelegramMusicSourceChangeInvalidatesOldButtons(t *testing.T) {
	b, requests := musicTestBot(t)
	if err := b.HandleUpdate(context.Background(), musicMessage("/search yt: song")); err != nil {
		t.Fatal(err)
	}
	old, _ := b.music.latest(1, 2)
	if err := b.HandleUpdate(context.Background(), musicCallback(old, "source", "bc")); err != nil {
		t.Fatal(err)
	}
	next, _ := b.music.latest(1, 2)
	if next.id == old.id || next.source != "bc" || next.tracks[0].Title != "bc:song #1" {
		t.Fatalf("wrong new source: %+v", next)
	}
	if err := b.HandleUpdate(context.Background(), musicCallback(old, "get", "0")); err != nil {
		t.Fatal(err)
	}
	if (*requests)[len(*requests)-1].text != resources.Get("telegram.music.expired") {
		t.Fatal("old button selected a replacement track")
	}
}

func TestTelegramMusicFailuresAndBusy(t *testing.T) {
	for _, mode := range []string{"search", "empty", "download", "size", "duration", "busy", "play expired", "other bot"} {
		t.Run(mode, func(t *testing.T) {
			b, requests := musicTestBot(t)
			command := "/play song"
			want := "telegram.music.download_failed"
			switch mode {
			case "search":
				b.music.search = func(context.Context, string, int) ([]music.Track, error) { return nil, fmt.Errorf("network failed") }
				want = "telegram.music.search_failed"
			case "empty":
				b.music.search = func(context.Context, string, int) ([]music.Track, error) { return nil, nil }
				want = "music.empty"
			case "download":
				b.music.download = func(context.Context, music.Track) (downloader.Media, error) {
					return downloader.Media{}, fmt.Errorf("download failed")
				}
			case "size":
				b.music.download = func(context.Context, music.Track) (downloader.Media, error) {
					return downloader.Media{}, downloader.ErrMusicTooLarge
				}
				want = "telegram.music.too_large"
			case "duration":
				b.music.download = func(context.Context, music.Track) (downloader.Media, error) {
					return downloader.Media{}, downloader.ErrMusicTooLong
				}
				want = "telegram.music.too_long"
			case "busy":
				b.music.busy = true
				want = "telegram.music.busy"
			case "play expired":
				command = "/play 3"
				want = "telegram.music.expired"
			case "other bot":
				command = "/search@different_bot song"
			}
			if err := b.HandleUpdate(context.Background(), musicMessage(command)); err != nil {
				t.Fatal(err)
			}
			if mode == "other bot" {
				if len(*requests) != 0 {
					t.Fatal("answered another bot's command")
				}
				return
			}
			if len(*requests) == 0 || (*requests)[len(*requests)-1].text != resources.Get(want) {
				t.Fatalf("missing failure feedback: %+v", *requests)
			}
		})
	}
}
