package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmionya/nyande-bot/internal/telegram"
)

func TestGameCallbacksAreBoundToCurrentMessageAndOpponent(t *testing.T) {
	for _, game := range []struct {
		command, accept, move string
		pending               func(*Bot) (gameChallenge, bool)
		activeMessage         func(*Bot) int
		moved                 func(*Bot) bool
	}{
		{"ttt", "ttt_accept:", "ttt:0",
			func(b *Bot) (gameChallenge, bool) { p, ok := b.tttPending[-1]; return p, ok },
			func(b *Bot) int {
				if g := b.tttGames[-1]; g != nil {
					return g.MessageID
				}
				return 0
			},
			func(b *Bot) bool { return b.tttGames[-1].Board[0] != tttEmpty }},
		{"checkers", "chk_accept:", "chk:sel:5:0",
			func(b *Bot) (gameChallenge, bool) { p, ok := b.checkersPending[-1]; return p, ok },
			func(b *Bot) int {
				if g := b.checkers[-1]; g != nil {
					return g.MessageID
				}
				return 0
			},
			func(b *Bot) bool { return b.checkers[-1].Selected != nil }},
	} {
		t.Run(game.command, func(t *testing.T) {
			b := &Bot{telegram: telegram.New("test"), state: NewState(),
				tttGames: make(map[int64]*ticTacToeGame), tttPending: make(map[int64]gameChallenge),
				checkers: make(map[int64]*checkersGame), checkersPending: make(map[int64]gameChallenge)}
			nextID, edits := 100, 0
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
				var result any = true
				switch filepath.Base(r.URL.Path) {
				case "sendMessage":
					result = telegram.Message{MessageID: nextID}
					nextID++
				case "editMessageText":
					edits++
				case "answerCallbackQuery":
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				body, err := json.Marshal(map[string]any{"ok": true, "result": result})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})
			start := func(player, opponent int64) int {
				t.Helper()
				message := &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: -1, Type: "supergroup"}, From: &telegram.User{ID: player}, Text: "/" + game.command}
				if opponent != 0 {
					message.ReplyToMessage = &telegram.Message{From: &telegram.User{ID: opponent}}
				}
				if err := b.HandleUpdate(context.Background(), telegram.Update{Message: message}); err != nil {
					t.Fatal(err)
				}
				return nextID - 1
			}
			click := func(player int64, messageID int, data string) {
				t.Helper()
				callback := &telegram.CallbackQuery{ID: "click", From: telegram.User{ID: player}, Data: data,
					Message: &telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: -1, Type: "supergroup"}}}
				if err := b.HandleUpdate(context.Background(), telegram.Update{CallbackQuery: callback}); err != nil {
					t.Fatal(err)
				}
			}
			oldChallenge := start(1, 2)
			currentChallenge := start(3, 4)
			click(2, oldChallenge, game.accept+"2")
			click(2, currentChallenge, game.accept+"2")
			// Even the expected opponent cannot accept through a different message.
			click(4, oldChallenge, game.accept+"4")
			pending, ok := game.pending(b)
			if !ok || pending.MessageID != currentChallenge || pending.Players != [2]int64{3, 4} || game.activeMessage(b) != 0 || edits != 0 {
				t.Fatal("stale or wrong-user callback consumed the current challenge")
			}
			click(4, currentChallenge, game.accept+"4")
			if game.activeMessage(b) != currentChallenge || edits != 1 {
				t.Fatal("current opponent could not accept the current challenge")
			}
			pendingChallenge := start(3, 4)
			currentBoard := start(3, 0)
			if _, ok := game.pending(b); ok {
				t.Fatal("starting a new bot game left a pending challenge alive")
			}
			click(4, pendingChallenge, game.accept+"4")
			click(3, currentChallenge, game.move)
			if game.activeMessage(b) != currentBoard || game.moved(b) || edits != 1 {
				t.Fatal("an old button changed the replacement game")
			}
			click(3, currentBoard, game.move)
			if !game.moved(b) || edits != 2 {
				t.Fatal("current game buttons no longer work")
			}
		})
	}
}
