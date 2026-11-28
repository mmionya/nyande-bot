package bot

import (
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestStatsCommandSendsPNGReply(t *testing.T) {
	b := concurrentTestBot()
	b.state.MediaTotal.Store(9)
	b.state.MediaErrors.Store(2)
	message := &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 123, Type: "private"}, Text: "/stats"}
	var methods []string
	downloadedAnimationTransport(t, func(request *http.Request) (*http.Response, error) {
		method := filepath.Base(request.URL.Path)
		methods = append(methods, method)
		if method != "sendPhoto" {
			t.Errorf("stats used %s instead of sendPhoto", method)
			return nil, errors.New("unexpected method")
		}
		if err := request.ParseMultipartForm(16 << 20); err != nil {
			t.Fatal(err)
		}
		defer request.MultipartForm.RemoveAll()
		if request.FormValue("chat_id") != "123" || request.FormValue("caption") != "" {
			t.Errorf("unexpected photo metadata: %v", request.MultipartForm.Value)
		}
		var reply struct {
			MessageID int `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(request.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != message.MessageID {
			t.Errorf("photo did not reply to the stats command: reply=%+v err=%v", reply, err)
		}
		file, header, err := request.FormFile("photo")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if header.Filename != "stats.png" || header.Header.Get("Content-Type") != "image/png" {
			t.Errorf("unexpected photo file: %s %s", header.Filename, header.Header.Get("Content-Type"))
		}
		card, err := png.Decode(file)
		if err != nil || card.Bounds().Empty() {
			t.Fatalf("stats did not send a valid nonempty PNG: %v", err)
		}
		return statsTestResponse(http.StatusOK, `{"ok":true,"result":{"message_id":100}}`), nil
	})
	if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{"sendPhoto"}) {
		t.Fatalf("unexpected requests: %v", methods)
	}
	if b.state.MediaTotal.Load() != 9 || b.state.MediaErrors.Load() != 2 {
		t.Fatal("the stats card changed download counters")
	}
}

func TestStatsCommandOnlyFallsBackAfterPhotoRejection(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		err      error
		fallback bool
	}{
		{name: "photo rejected", status: 400, body: `{"ok":false,"error_code":400,"description":"PHOTO_INVALID_DIMENSIONS"}`, fallback: true},
		{name: "connection lost", err: io.ErrUnexpectedEOF},
		{name: "invalid response", status: 502, body: `upstream response lost`},
		{name: "server error", status: 500, body: `{"ok":false,"error_code":500,"description":"Internal Server Error"}`},
		{name: "lost confirmation", status: 200, body: `{"ok":true,"result":"lost confirmation"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := concurrentTestBot()
			b.state.MediaTotal.Store(9)
			b.state.MediaErrors.Store(2)
			message := &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 123, Type: "private"}, Text: "/stats"}
			var methods []string
			downloadedAnimationTransport(t, func(request *http.Request) (*http.Response, error) {
				method := filepath.Base(request.URL.Path)
				methods = append(methods, method)
				switch method {
				case "sendPhoto":
					// Other chats can finish work while the upload is in flight.
					// The fallback must still describe the original card snapshot.
					b.state.MediaTotal.Store(100)
					if test.err != nil {
						return nil, test.err
					}
					return statsTestResponse(test.status, test.body), nil
				case "sendMessage":
					var payload struct {
						ChatID int64  `json:"chat_id"`
						Text   string `json:"text"`
						Reply  struct {
							MessageID int `json:"message_id"`
						} `json:"reply_parameters"`
					}
					if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.ChatID != message.Chat.ID || payload.Reply.MessageID != message.MessageID || !strings.HasPrefix(payload.Text, "Статистика :3\n") {
						t.Errorf("unexpected stats fallback: %+v", payload)
					}
					if !strings.Contains(payload.Text, "Медиа всего     9\n") || !strings.Contains(payload.Text, "Ошибок          2 (22.2%)") {
						t.Errorf("fallback resampled counters after the upload: %s", payload.Text)
					}
					return statsTestResponse(http.StatusOK, `{"ok":true,"result":{"message_id":100}}`), nil
				default:
					t.Errorf("unexpected request: %s", method)
					return nil, errors.New("unexpected method")
				}
			})
			err := b.HandleUpdate(context.Background(), telegram.Update{Message: message})
			if (err == nil) != test.fallback {
				t.Fatalf("fallback=%v err=%v", test.fallback, err)
			}
			wantMethods := []string{"sendPhoto"}
			if test.fallback {
				wantMethods = append(wantMethods, "sendMessage")
			}
			if !reflect.DeepEqual(methods, wantMethods) {
				t.Fatalf("upload repeated or unsafe fallback: methods=%v want=%v", methods, wantMethods)
			}
		})
	}
}

func TestStatsSnapshotBoundsConcurrentCounters(t *testing.T) {
	b := concurrentTestBot()
	b.state.StartedAt = time.Now().Add(time.Hour)
	b.state.MessagesTotal.Store(3)
	b.state.CommandsUsed.Store(4)
	b.state.MediaTotal.Store(2)
	b.state.MediaErrors.Store(3)
	b.state.LLMCalls.Store(-1)
	b.state.LLMErrors.Store(1)
	b.state.TrackChat(123)
	want := statsSnapshot{Chats: 1, Messages: 3, Commands: 3, Media: 2, MediaErrors: 2}
	if got := b.statsSnapshot(); got != want {
		t.Fatalf("invalid snapshot: got=%+v want=%+v", got, want)
	}
}

func statsTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
