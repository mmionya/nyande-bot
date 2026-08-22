package discordbot

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/hyphentae/nyande-bot/internal/config"
)

func TestNewConfiguresRequiredGatewayIntents(t *testing.T) {
	instance, err := New(config.Config{DiscordToken: "test-token", DiscordPrefix: "!"})
	if err != nil {
		t.Fatal(err)
	}
	want := discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
	if instance.session.Identify.Intents != want {
		t.Fatalf("gateway intents = %d, want %d", instance.session.Identify.Intents, want)
	}
}

func TestNewRejectsEmptyToken(t *testing.T) {
	if _, err := New(config.Config{}); err == nil {
		t.Fatal("expected an empty token error")
	}
}

func TestExtractURLsNormalizesAndDeduplicates(t *testing.T) {
	text := "one <https://www.pinterest.com/pin/123/>, two www.xiaohongshu.com/explore/456! and https://www.pinterest.com/pin/123/"
	want := []string{
		"https://www.pinterest.com/pin/123/",
		"https://www.xiaohongshu.com/explore/456",
	}
	if got := extractURLs(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("extractURLs() = %#v, want %#v", got, want)
	}
}

func TestParseCommandUsesConfiguredPrefix(t *testing.T) {
	if got, ok := parseCommand("?HeLp now", "?"); !ok || got != "help" {
		t.Fatalf("parseCommand() = %q, %v", got, ok)
	}
	if _, ok := parseCommand("!help", "?"); ok {
		t.Fatal("a different prefix must not match")
	}
}

func TestTruncateMessageCountsRunes(t *testing.T) {
	got := truncateMessage(strings.Repeat("я", 10), 5)
	if got != "яяяя…" {
		t.Fatalf("truncateMessage() = %q", got)
	}
}

func TestDownloadErrorTextDoesNotExposeInternalError(t *testing.T) {
	got := downloadErrorText(assertionError("request failed with secret-token"))
	if strings.Contains(got, "secret-token") {
		t.Fatalf("internal error leaked: %q", got)
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
