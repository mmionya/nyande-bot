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
	checkerEmpty = "·"
	checkerLight = " "
	checkerP1    = "r"
	checkerP2    = "b"
)

type checkerPosition struct {
	Row int
	Col int
}

type checkerMove struct {
	From checkerPosition
	To   checkerPosition
}

type checkersGame struct {
	Player1  int64
	Player2  int64
	Board    [8][8]string
	Current  string
	Selected *checkerPosition
	Over     bool
	Winner   string
}

func newCheckers(player1, player2 int64) *checkersGame {
	game := &checkersGame{Player1: player1, Player2: player2, Current: checkerP1}
	for row := 0; row < 8; row++ {
		for column := 0; column < 8; column++ {
			if (row+column)%2 == 0 {
				game.Board[row][column] = checkerLight
			} else {
				game.Board[row][column] = checkerEmpty
				if row < 3 {
					game.Board[row][column] = checkerP2
				} else if row > 4 {
					game.Board[row][column] = checkerP1
				}
			}
		}
	}
	return game
}

func (g *checkersGame) vsBot() bool {
	return g.Player2 == 0
}

func (g *checkersGame) currentPlayer() int64 {
	if g.Current == checkerP1 {
		return g.Player1
	}
	return g.Player2
}

func checkerOwner(piece string) string {
	switch strings.ToLower(piece) {
	case checkerP1:
		return checkerP1
	case checkerP2:
		return checkerP2
	default:
		return ""
	}
}

func checkerInside(row, column int) bool {
	return row >= 0 && row < 8 && column >= 0 && column < 8
}

var diagonalDirections = [][2]int{{-1, -1}, {-1, 1}, {1, -1}, {1, 1}}

func checkerCaptures(board [8][8]string, from checkerPosition, player string) []checkerMove {
	result := []checkerMove{}
	if !checkerInside(from.Row, from.Col) || checkerOwner(board[from.Row][from.Col]) != player {
		return result
	}
	for _, direction := range diagonalDirections {
		middleRow, middleCol := from.Row+direction[0], from.Col+direction[1]
		targetRow, targetCol := from.Row+2*direction[0], from.Col+2*direction[1]
		if checkerInside(targetRow, targetCol) && checkerOwner(board[middleRow][middleCol]) != "" &&
			checkerOwner(board[middleRow][middleCol]) != player && board[targetRow][targetCol] == checkerEmpty {
			result = append(result, checkerMove{From: from, To: checkerPosition{targetRow, targetCol}})
		}
	}
	return result
}

func checkerLegalMoves(game *checkersGame) []checkerMove {
	if game.Over {
		return nil
	}
	if game.Selected != nil {
		if captures := checkerCaptures(game.Board, *game.Selected, game.Current); len(captures) > 0 {
			return captures
		}
	}
	captures := []checkerMove{}
	for row := 0; row < 8; row++ {
		for column := 0; column < 8; column++ {
			if checkerOwner(game.Board[row][column]) == game.Current {
				captures = append(captures, checkerCaptures(game.Board, checkerPosition{row, column}, game.Current)...)
			}
		}
	}
	if len(captures) > 0 {
		return captures
	}
	moves := []checkerMove{}
	for row := 0; row < 8; row++ {
		for column := 0; column < 8; column++ {
			piece := game.Board[row][column]
			if checkerOwner(piece) != game.Current {
				continue
			}
			directions := diagonalDirections
			if piece == strings.ToLower(piece) {
				if game.Current == checkerP1 {
					directions = [][2]int{{-1, -1}, {-1, 1}}
				} else {
					directions = [][2]int{{1, -1}, {1, 1}}
				}
			}
			for _, direction := range directions {
				target := checkerPosition{row + direction[0], column + direction[1]}
				if checkerInside(target.Row, target.Col) && game.Board[target.Row][target.Col] == checkerEmpty {
					moves = append(moves, checkerMove{From: checkerPosition{row, column}, To: target})
				}
			}
		}
	}
	return moves
}

func checkerApply(game *checkersGame, movement checkerMove) bool {
	legal := checkerLegalMoves(game)
	found := false
	for _, candidate := range legal {
		if candidate == movement {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	piece := game.Board[movement.From.Row][movement.From.Col]
	game.Board[movement.From.Row][movement.From.Col] = checkerEmpty
	game.Board[movement.To.Row][movement.To.Col] = piece
	captured := abs(movement.To.Row-movement.From.Row) == 2
	if captured {
		game.Board[(movement.From.Row+movement.To.Row)/2][(movement.From.Col+movement.To.Col)/2] = checkerEmpty
	}
	if piece == checkerP1 && movement.To.Row == 0 {
		game.Board[movement.To.Row][movement.To.Col] = strings.ToUpper(piece)
	}
	if piece == checkerP2 && movement.To.Row == 7 {
		game.Board[movement.To.Row][movement.To.Col] = strings.ToUpper(piece)
	}
	if captured && len(checkerCaptures(game.Board, movement.To, game.Current)) > 0 {
		selected := movement.To
		game.Selected = &selected
	} else {
		game.Selected = nil
		if game.Current == checkerP1 {
			game.Current = checkerP2
		} else {
			game.Current = checkerP1
		}
	}
	checkerFinish(game)
	return true
}

func checkerFinish(game *checkersGame) {
	count1, count2 := 0, 0
	for _, row := range game.Board {
		for _, piece := range row {
			if checkerOwner(piece) == checkerP1 {
				count1++
			} else if checkerOwner(piece) == checkerP2 {
				count2++
			}
		}
	}
	if count1 == 0 || count2 == 0 {
		game.Over = true
		if count1 > 0 {
			game.Winner = checkerP1
		} else {
			game.Winner = checkerP2
		}
		return
	}
	if len(checkerLegalMoves(game)) == 0 {
		game.Over = true
		if game.Current == checkerP1 {
			game.Winner = checkerP2
		} else {
			game.Winner = checkerP1
		}
	}
}

func (b *Bot) startCheckers(ctx context.Context, message *telegram.Message) error {
	if message.From == nil {
		return nil
	}
	if opponent := challengedUser(message); opponent != nil && opponent.ID != message.From.ID && !opponent.IsBot {
		b.checkersMu.Lock()
		b.checkersPending[message.Chat.ID] = [2]int64{message.From.ID, opponent.ID}
		b.checkersMu.Unlock()
		text := resources.Format("checkers.challenge", map[string]any{"challenger": message.From.DisplayName(), "opponent": opponent.DisplayName()})
		markup := map[string]any{"inline_keyboard": [][]map[string]any{{{
			"text": resources.Get("game.accept"), "callback_data": fmt.Sprintf("chk_accept:%d", opponent.ID),
		}}}}
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, text, message.MessageID, markup)
		return err
	}
	game := newCheckers(message.From.ID, 0)
	b.checkersMu.Lock()
	b.checkers[message.Chat.ID] = game
	b.checkersMu.Unlock()
	_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("checkers.started_bot"), message.MessageID, checkersKeyboard(game))
	return err
}

func (b *Bot) handleCheckersCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	if callback.Message == nil {
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	chatID := callback.Message.Chat.ID
	if strings.HasPrefix(callback.Data, "chk_accept:") {
		expected, _ := strconv.ParseInt(strings.TrimPrefix(callback.Data, "chk_accept:"), 10, 64)
		if callback.From.ID != expected {
			return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("game.challenge.not_for_you"), "", true)
		}
		b.checkersMu.Lock()
		pending, exists := b.checkersPending[chatID]
		if exists {
			delete(b.checkersPending, chatID)
			b.checkers[chatID] = newCheckers(pending[0], pending[1])
		}
		game := b.checkers[chatID]
		b.checkersMu.Unlock()
		if !exists || game == nil {
			return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("game.challenge.expired"), "", true)
		}
		if err := b.telegram.EditMessageText(ctx, chatID, callback.Message.MessageID, resources.Get("checkers.started_players"), checkersKeyboard(game)); err != nil {
			return err
		}
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	if callback.Data == "chk:noop" {
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}

	b.checkersMu.Lock()
	game := b.checkers[chatID]
	if game == nil || game.Over {
		b.checkersMu.Unlock()
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("checkers.no_game"), "", true)
	}
	if expected := game.currentPlayer(); expected != 0 && expected != callback.From.ID {
		b.checkersMu.Unlock()
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("game.not_your_turn"), "", true)
	}
	parts := strings.Split(callback.Data, ":")
	switch {
	case len(parts) == 4 && parts[1] == "sel":
		row, _ := strconv.Atoi(parts[2])
		column, _ := strconv.Atoi(parts[3])
		available := false
		for _, movement := range checkerLegalMoves(game) {
			if movement.From == (checkerPosition{row, column}) {
				available = true
				break
			}
		}
		if !available {
			b.checkersMu.Unlock()
			return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("checkers.no_moves"), "", true)
		}
		selected := checkerPosition{row, column}
		game.Selected = &selected
	case len(parts) == 4 && parts[1] == "move" && game.Selected != nil:
		row, _ := strconv.Atoi(parts[2])
		column, _ := strconv.Atoi(parts[3])
		if !checkerApply(game, checkerMove{From: *game.Selected, To: checkerPosition{row, column}}) {
			b.checkersMu.Unlock()
			return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("checkers.invalid_move"), "", true)
		}
		if game.vsBot() && !game.Over && game.Current == checkerP2 {
			for game.Current == checkerP2 && !game.Over {
				moves := checkerLegalMoves(game)
				if len(moves) == 0 {
					checkerFinish(game)
					break
				}
				checkerApply(game, chooseBotCheckerMove(moves))
			}
		}
	default:
		b.checkersMu.Unlock()
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
	text := checkersStatus(game)
	keyboard := checkersKeyboard(game)
	if game.Over {
		delete(b.checkers, chatID)
	}
	b.checkersMu.Unlock()
	if err := b.telegram.EditMessageText(ctx, chatID, callback.Message.MessageID, text, keyboard); err != nil {
		return err
	}
	return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
}

func chooseBotCheckerMove(moves []checkerMove) checkerMove {
	for _, movement := range moves {
		if abs(movement.To.Row-movement.From.Row) == 2 {
			return movement
		}
	}
	best := moves[0]
	bestScore := -100
	for _, movement := range moves {
		score := movement.To.Row
		if movement.To.Col >= 2 && movement.To.Col <= 5 {
			score += 2
		}
		if score > bestScore {
			bestScore = score
			best = movement
		}
	}
	return best
}

func checkersKeyboard(game *checkersGame) map[string]any {
	targets := map[checkerPosition]bool{}
	if game.Selected != nil {
		for _, movement := range checkerLegalMoves(game) {
			if movement.From == *game.Selected {
				targets[movement.To] = true
			}
		}
	}
	rows := make([][]map[string]any, 8)
	for row := 0; row < 8; row++ {
		rows[row] = make([]map[string]any, 8)
		for column := 0; column < 8; column++ {
			position := checkerPosition{row, column}
			piece := game.Board[row][column]
			text := checkerGlyph(piece)
			callback := "chk:noop"
			if game.Selected != nil && *game.Selected == position {
				text = "◆"
			} else if targets[position] {
				text = "●"
				callback = fmt.Sprintf("chk:move:%d:%d", row, column)
			} else if !game.Over && checkerOwner(piece) == game.Current {
				callback = fmt.Sprintf("chk:sel:%d:%d", row, column)
			}
			rows[row][column] = map[string]any{"text": text, "callback_data": callback}
		}
	}
	return map[string]any{"inline_keyboard": rows}
}

func checkerGlyph(piece string) string {
	switch piece {
	case checkerP1:
		return "🔴"
	case checkerP2:
		return "🔵"
	case "R":
		return "🟥"
	case "B":
		return "🟦"
	case checkerLight:
		return " "
	default:
		return "·"
	}
}

func checkersStatus(game *checkersGame) string {
	if game.Over {
		if game.Winner == checkerP1 {
			return resources.Get("checkers.p1_won")
		}
		if game.vsBot() {
			return resources.Get("checkers.bot_won")
		}
		return resources.Get("checkers.p2_won")
	}
	if game.Selected != nil && len(checkerCaptures(game.Board, *game.Selected, game.Current)) > 0 {
		return resources.Get("checkers.continue_capture")
	}
	if game.Current == checkerP1 {
		return resources.Get("checkers.turn_p1")
	}
	return resources.Get("checkers.turn_p2")
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
