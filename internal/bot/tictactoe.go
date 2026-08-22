package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hyphentae/nyande-bot/internal/telegram"
	"github.com/hyphentae/nyande-bot/resources"
)

const (
	tttEmpty = "·"
	tttX     = "✕"
	tttO     = "○"
)

type ticTacToeGame struct {
	PlayerX int64
	PlayerO int64
	Board   [9]string
	Current string
	Over    bool
}

func newTicTacToe(playerX, playerO int64) *ticTacToeGame {
	game := &ticTacToeGame{PlayerX: playerX, PlayerO: playerO, Current: tttX}
	for index := range game.Board {
		game.Board[index] = tttEmpty
	}
	return game
}

func (g *ticTacToeGame) vsBot() bool {
	return g.PlayerO == 0
}

func (g *ticTacToeGame) player() int64 {
	if g.Current == tttX {
		return g.PlayerX
	}
	return g.PlayerO
}

func (g *ticTacToeGame) move(index int) bool {
	if g.Over || index < 0 || index >= len(g.Board) || g.Board[index] != tttEmpty {
		return false
	}
	g.Board[index] = g.Current
	if g.Current == tttX {
		g.Current = tttO
	} else {
		g.Current = tttX
	}
	return true
}

func (g *ticTacToeGame) winner() string {
	lines := [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}}
	for _, line := range lines {
		if g.Board[line[0]] != tttEmpty && g.Board[line[0]] == g.Board[line[1]] && g.Board[line[1]] == g.Board[line[2]] {
			g.Over = true
			return g.Board[line[0]]
		}
	}
	for _, cell := range g.Board {
		if cell == tttEmpty {
			return ""
		}
	}
	g.Over = true
	return "draw"
}

func (g *ticTacToeGame) botMove() int {
	if index := g.findWinning(tttO); index >= 0 {
		return index
	}
	if index := g.findWinning(tttX); index >= 0 {
		return index
	}
	for _, index := range []int{4, 0, 2, 6, 8, 1, 3, 5, 7} {
		if g.Board[index] == tttEmpty {
			return index
		}
	}
	return -1
}

func (g *ticTacToeGame) findWinning(mark string) int {
	lines := [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}}
	for _, line := range lines {
		marks := 0
		empty := -1
		for _, index := range line {
			if g.Board[index] == mark {
				marks++
			} else if g.Board[index] == tttEmpty {
				empty = index
			}
		}
		if marks == 2 && empty >= 0 {
			return empty
		}
	}
	return -1
}

func (b *Bot) startTicTacToe(ctx context.Context, message *telegram.Message) error {
	if message.From == nil {
		return nil
	}
	if opponent := challengedUser(message); opponent != nil && opponent.ID != message.From.ID && !opponent.IsBot {
		b.tttMu.Lock()
		b.tttPending[message.Chat.ID] = [2]int64{message.From.ID, opponent.ID}
		b.tttMu.Unlock()
		text := resources.Format("ttt.challenge", map[string]any{"challenger": message.From.DisplayName(), "opponent": opponent.DisplayName()})
		markup := map[string]any{"inline_keyboard": [][]map[string]any{{{
			"text": resources.Get("game.accept"), "callback_data": fmt.Sprintf("ttt_accept:%d", opponent.ID),
		}}}}
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, markup)
		return err
	}
	game := newTicTacToe(message.From.ID, 0)
	b.tttMu.Lock()
	b.tttGames[message.Chat.ID] = game
	b.tttMu.Unlock()
	_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("ttt.started_bot"), message.MessageID, tttKeyboard(game))
	return err
}

func (b *Bot) handleTicTacToeCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	if callback.Message == nil {
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	chatID := callback.Message.Chat.ID
	if strings.HasPrefix(callback.Data, "ttt_accept:") {
		expected, _ := strconv.ParseInt(strings.TrimPrefix(callback.Data, "ttt_accept:"), 10, 64)
		if callback.From.ID != expected {
			return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("game.challenge.not_for_you"), "", true)
		}
		b.tttMu.Lock()
		pending, exists := b.tttPending[chatID]
		if exists {
			delete(b.tttPending, chatID)
			b.tttGames[chatID] = newTicTacToe(pending[0], pending[1])
		}
		game := b.tttGames[chatID]
		b.tttMu.Unlock()
		if !exists || game == nil {
			return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("game.challenge.expired"), "", true)
		}
		if err := b.telegram.EditMessageText(ctx, chatID, callback.Message.MessageID, resources.Get("ttt.started_players"), tttKeyboard(game)); err != nil {
			return err
		}
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	if callback.Data == "ttt:noop" {
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	index, err := strconv.Atoi(strings.TrimPrefix(callback.Data, "ttt:"))
	if err != nil {
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	b.tttMu.Lock()
	game := b.tttGames[chatID]
	if game == nil {
		b.tttMu.Unlock()
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("ttt.no_game"), "", true)
	}
	expected := game.player()
	if expected != 0 && expected != callback.From.ID {
		b.tttMu.Unlock()
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("game.not_your_turn"), "", true)
	}
	if !game.move(index) {
		b.tttMu.Unlock()
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("ttt.cell_busy"), "", true)
	}
	winner := game.winner()
	if winner == "" && game.vsBot() && game.Current == tttO {
		if botIndex := game.botMove(); botIndex >= 0 {
			game.move(botIndex)
		}
		winner = game.winner()
	}
	text := tttStatus(game, winner)
	keyboard := tttKeyboard(game)
	if winner != "" {
		delete(b.tttGames, chatID)
	}
	b.tttMu.Unlock()
	if err := b.telegram.EditMessageText(ctx, chatID, callback.Message.MessageID, text, keyboard); err != nil {
		return err
	}
	return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
}

func tttKeyboard(game *ticTacToeGame) map[string]any {
	rows := make([][]map[string]any, 3)
	for row := 0; row < 3; row++ {
		rows[row] = make([]map[string]any, 3)
		for column := 0; column < 3; column++ {
			index := row*3 + column
			callback := "ttt:noop"
			if !game.Over && game.Board[index] == tttEmpty {
				callback = fmt.Sprintf("ttt:%d", index)
			}
			rows[row][column] = map[string]any{"text": game.Board[index], "callback_data": callback}
		}
	}
	return map[string]any{"inline_keyboard": rows}
}

func tttStatus(game *ticTacToeGame, winner string) string {
	switch winner {
	case "draw":
		return resources.Get("ttt.draw")
	case tttX:
		return resources.Get("ttt.x_won")
	case tttO:
		if game.vsBot() {
			return resources.Get("ttt.bot_won")
		}
		return resources.Get("ttt.o_won")
	default:
		if game.Current == tttX {
			return resources.Get("ttt.turn_x")
		}
		return resources.Get("ttt.turn_o")
	}
}

func challengedUser(message *telegram.Message) *telegram.User {
	for _, entity := range message.Entities {
		if entity.Type == "text_mention" && entity.User != nil {
			return entity.User
		}
	}
	if message.ReplyToMessage != nil && message.ReplyToMessage.From != nil {
		return message.ReplyToMessage.From
	}
	return nil
}
