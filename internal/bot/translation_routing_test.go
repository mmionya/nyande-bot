package bot

import "testing"

func TestTwitterTranslationRouting(t *testing.T) {
	for _, test := range []struct {
		text string
		want bool
	}{
		{"переведи " + twitterIntegrationURL, true},
		{"Переведите на казахский: " + twitterIntegrationURL, true},
		{"Нянде, можешь перевести этот пост? " + twitterIntegrationURL, true},
		{"Please translate into English: " + twitterIntegrationURL, true},
		{twitterIntegrationURL + " переведи, пожалуйста", true},
		{twitterIntegrationURL, false},
		{"скачай " + twitterIntegrationURL, false},
		{"https://x.com/translate/status/123", false},
		{"untranslated " + twitterIntegrationURL, false},
		{"translate https://example.com/post", false},
	} {
		t.Run(test.text, func(t *testing.T) {
			if got := twitterTranslationRequest(twitterIntegrationMessage(test.text)) != ""; got != test.want {
				t.Fatalf("translation routing=%v, want %v", got, test.want)
			}
		})
	}
}

func TestTranslateCommandPreservesSourceFormatting(t *testing.T) {
	command, arguments, ok := parseCommand("/translate@nyande_test_bot en First line\n\n  indented\n    code", "nyande_test_bot")
	if !ok || command != "translate" || arguments != "en First line\n\n  indented\n    code" {
		t.Fatalf("command=%q arguments=%q ok=%v", command, arguments, ok)
	}
}
