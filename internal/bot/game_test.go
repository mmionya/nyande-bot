package bot

import "testing"

func TestTicTacToeBotBlocksWinningMove(t *testing.T) {
	game := newTicTacToe(1, 0)
	game.Board = [9]string{tttX, tttX, tttEmpty, tttEmpty, tttO, tttEmpty, tttEmpty, tttEmpty, tttEmpty}
	game.Current = tttO
	if move := game.botMove(); move != 2 {
		t.Fatalf("bot should block square 2, got %d", move)
	}
}

func TestCheckersMandatoryCapture(t *testing.T) {
	game := newCheckers(1, 0)
	for row := range game.Board {
		for column := range game.Board[row] {
			if (row+column)%2 == 0 {
				game.Board[row][column] = checkerLight
			} else {
				game.Board[row][column] = checkerEmpty
			}
		}
	}
	game.Board[5][0] = checkerP1
	game.Board[4][1] = checkerP2
	game.Board[5][4] = checkerP1
	moves := checkerLegalMoves(game)
	if len(moves) != 1 || moves[0].To != (checkerPosition{3, 2}) {
		t.Fatalf("expected mandatory capture, got %#v", moves)
	}
}
