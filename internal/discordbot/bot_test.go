package discordbot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/resources"
)

func TestNewConfiguresRequiredGatewayIntents(t *testing.T) {
	instance, err := New(config.Config{DiscordToken: "test-token", DiscordPrefix: "!"})
	if err != nil {
		t.Fatal(err)
	}
	want := discordgo.IntentsGuilds | discordgo.IntentsGuildVoiceStates | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
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

func TestCommandsSendResourceText(t *testing.T) {
	for _, command := range []string{"help", "ping", "stats"} {
		t.Run(command, func(t *testing.T) {
			b, err := New(config.Config{DiscordToken: "test-token", DiscordPrefix: "ня!"})
			if err != nil {
				t.Fatal(err)
			}
			want := resources.Get("discord_ping")
			switch command {
			case "help":
				want = resources.Format("discord_help", map[string]any{"prefix": "ня!"})
			case "stats":
				want = "Статистика Discord\nАптайм: 0s\nКаналы: 0\nСообщения: 0\nКоманды: 0\nМедиа: 0 (успешно: 0, ошибок: 0, 0.0%)"
			}
			calls := 0
			b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				var payload discordgo.MessageSend
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload.Content != want {
					t.Errorf("response = %q, want %q", payload.Content, want)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"reply"}`))}, nil
			})}
			message := &discordgo.Message{ID: "message", ChannelID: "channel"}
			if !b.handleCommand(b.session, message, command) || calls != 1 || b.commands.Load() != 1 {
				t.Fatalf("command not handled once: requests=%d, commands=%d", calls, b.commands.Load())
			}
		})
	}
}

type discordRoundTripFunc func(*http.Request) (*http.Response, error)

func (f discordRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStatsText(t *testing.T) {
	for _, tc := range []struct {
		name          string
		total, errors int64
		wantMedia     string
	}{
		{"empty", 0, 0, "0 (успешно: 0, ошибок: 0, 0.0%)"},
		{"success", 7, 0, "7 (успешно: 7, ошибок: 0, 0.0%)"},
		{"mixed", 3, 1, "3 (успешно: 2, ошибок: 1, 33.3%)"},
		{"failed", 5, 5, "5 (успешно: 0, ошибок: 5, 100.0%)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bot{startedAt: time.Now().Add(-time.Hour - 2*time.Minute - 3*time.Second), uniqueChannels: make(map[string]struct{})}
			b.trackChannel("one")
			b.trackChannel("one")
			b.trackChannel("two")
			b.messages.Store(42)
			b.commands.Store(9)
			b.mediaTotal.Store(tc.total)
			b.mediaErrors.Store(tc.errors)
			want := "Статистика Discord\nАптайм: 1h2m3s\nКаналы: 2\nСообщения: 42\nКоманды: 9\nМедиа: " + tc.wantMedia
			if got := b.statsText(); got != want {
				t.Fatalf("statsText() = %q, want %q", got, want)
			}
		})
	}
}

func TestDownloadErrorTextSelectsResource(t *testing.T) {
	for _, tc := range []struct {
		err error
		key string
	}{
		{assertionError("file TOO LARGE"), "discord_download_too_large"},
		{assertionError("file exceeds limit"), "discord_download_too_large"},
		{assertionError("HTTP 404"), "discord_download_not_found"},
		{assertionError("post NOT FOUND"), "discord_download_not_found"},
		{assertionError("request TIMEOUT"), "discord_download_timeout"},
		{context.DeadlineExceeded, "discord_download_timeout"},
		{fmt.Errorf("download: %w", context.DeadlineExceeded), "discord_download_timeout"},
		{assertionError("private publication"), "discord_download_failed"},
		{assertionError("unknown failure"), "discord_download_failed"},
		{assertionError("too large; 404; timeout"), "discord_download_too_large"},
		{assertionError("404; timeout"), "discord_download_not_found"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			if got, want := downloadErrorText(tc.err), resources.Get(tc.key); got != want {
				t.Fatalf("downloadErrorText() = %q, want %q", got, want)
			}
		})
	}
}
