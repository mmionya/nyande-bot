package bot

import (
	"bytes"
	"image"
	"testing"
	"time"
)

func TestEvaluateWordleGuessHandlesDuplicateLetters(t *testing.T) {
	result := evaluateWordleGuess("banal", "banna")
	want := [5]int{wordleCorrect, wordleCorrect, wordleCorrect, wordleAbsent, wordlePresent}
	if result != want {
		t.Fatalf("unexpected result: got %v, want %v", result, want)
	}
}

func TestNormalizeWordleWord(t *testing.T) {
	word, ok := normalizeWordleWord("  APPLE ")
	if !ok || word != "apple" {
		t.Fatalf("unexpected normalized word %q, valid=%t", word, ok)
	}
	for _, invalid := range []string{"домик", "four", "word!", "two words"} {
		if _, valid := normalizeWordleWord(invalid); valid {
			t.Fatalf("expected %q to be invalid", invalid)
		}
	}
}

func TestWordleNumberMatchesReferenceEpoch(t *testing.T) {
	day, number, word := currentWordle("UTC", time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC))
	if day != "2026-08-01" || number != 1869 {
		t.Fatalf("got day=%s number=%d", day, number)
	}
	if _, valid := normalizeWordleWord(word); !valid {
		t.Fatalf("daily word %q is invalid", word)
	}
}

func TestWordleDictionaryContainsFiveLetterWords(t *testing.T) {
	seen := make(map[string]bool, len(wordleWords))
	for _, word := range wordleWords {
		if _, valid := normalizeWordleWord(word); !valid {
			t.Errorf("invalid dictionary word %q", word)
		}
		if seen[word] {
			t.Errorf("duplicate dictionary word %q", word)
		}
		seen[word] = true
	}
}

func TestRenderWordleCardProducesPNG(t *testing.T) {
	game := wordleGame{
		Number: 1869, Finished: true,
		Guesses: []wordleGuess{
			{Result: [5]int{wordlePresent, wordleAbsent, wordleAbsent, wordleCorrect, wordleAbsent}},
			{Result: [5]int{wordleAbsent, wordleAbsent, wordleAbsent, wordlePresent, wordleAbsent}},
			{Result: [5]int{wordleAbsent, wordleCorrect, wordleCorrect, wordleCorrect, wordleCorrect}},
			{Result: [5]int{wordleAbsent, wordleCorrect, wordleCorrect, wordleCorrect, wordleCorrect}},
			{Result: [5]int{wordleAbsent, wordleCorrect, wordleCorrect, wordleCorrect, wordleCorrect}},
			{Result: [5]int{wordleCorrect, wordleCorrect, wordleCorrect, wordleCorrect, wordleCorrect}},
		},
	}
	data, err := renderWordleCard(game, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "png" || decoded.Bounds().Dx() != 1024 || decoded.Bounds().Dy() != 560 {
		t.Fatalf("unexpected image: format=%s bounds=%v", format, decoded.Bounds())
	}
}
