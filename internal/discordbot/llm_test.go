package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/resources"
)

type fakeLanguageModel struct {
	enabled  bool
	requests []llm.Request
	answer   string
	err      error
	resetIDs []int64
}

func (f *fakeLanguageModel) Enabled() bool { return f.enabled }
func (f *fakeLanguageModel) Ask(ctx context.Context, request llm.Request) (string, error) {
	f.requests = append(f.requests, request)
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("missing request deadline")
	}
	return f.answer, f.err
}
func (f *fakeLanguageModel) Reset(id int64) bool { f.resetIDs = append(f.resetIDs, id); return true }

func TestDiscordLLMTriggers(t *testing.T) {
	b, err := New(config.Config{DiscordToken: "test", DiscordPrefix: "!", LLMTriggerWords: []string{"мяу", " "}})
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeLanguageModel{enabled: true}
	b.llm = model
	b.session.State.User = &discordgo.User{ID: "99"}
	for _, tc := range []struct {
		name    string
		message discordgo.Message
		want    bool
	}{
		{"DM", discordgo.Message{Content: "hello"}, true},
		{"empty", discordgo.Message{}, false},
		{"guild", discordgo.Message{GuildID: "1", Content: "hello"}, false},
		{"trigger", discordgo.Message{GuildID: "1", Content: "МЯУ привет"}, true},
		{"mention", discordgo.Message{GuildID: "1", Content: "<@99> hi", Mentions: []*discordgo.User{{ID: "99"}}}, true},
		{"other mention", discordgo.Message{GuildID: "1", Content: "hi", Mentions: []*discordgo.User{{ID: "98"}}}, false},
		{"reply", discordgo.Message{GuildID: "1", Content: "hi", ReferencedMessage: &discordgo.Message{Author: &discordgo.User{ID: "99"}}}, true},
		{"command", discordgo.Message{Content: "!unknown мяу"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.shouldAnswerLLM(b.session, &tc.message); got != tc.want {
				t.Fatalf("trigger = %v", got)
			}
		})
	}
	model.enabled = false
	if b.shouldAnswerLLM(b.session, &discordgo.Message{Content: "hello"}) {
		t.Fatal("disabled LLM triggered")
	}
}

func TestDiscordLLMRepliesAndReset(t *testing.T) {
	b, err := New(config.Config{DiscordToken: "test", DiscordPrefix: "!", LLMCooldown: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	b.session.State.User = &discordgo.User{ID: "99"}
	model := &fakeLanguageModel{enabled: true, answer: strings.Repeat("🙂", 1100) + " @everyone"}
	b.llm = model
	var sent []discordgo.MessageSend
	b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/typing") {
			var payload discordgo.MessageSend
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			sent = append(sent, payload)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"reply"}`))}, nil
	})}
	message := &discordgo.Message{ID: "1", ChannelID: "123", Content: "<@99> hello", Author: &discordgo.User{ID: "2", Username: "alice"}}
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: message})
	if len(model.requests) != 1 || model.requests[0].ChatID != 123 || model.requests[0].Text != " hello" || model.requests[0].UserName != "alice" {
		t.Fatalf("requests = %#v", model.requests)
	}
	if len(sent) != 2 || sent[0].Reference == nil || sent[1].Reference != nil {
		t.Fatalf("replies = %#v", sent)
	}
	var combined string
	for _, part := range sent {
		combined += part.Content
		if part.AllowedMentions == nil || len(part.AllowedMentions.Parse) != 0 || part.AllowedMentions.RepliedUser {
			t.Fatal("reply permits mentions")
		}
	}
	if combined != model.answer {
		t.Fatal("answer was truncated")
	}
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: message})
	if len(model.requests) != 1 {
		t.Fatal("cooldown ignored")
	}
	message.ChannelID = "456"
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: message})
	if len(model.requests) != 2 || model.requests[1].ChatID != 456 {
		t.Fatal("channels share cooldown or history ID")
	}
	message.ChannelID = "123"
	if got := b.resetLLM(message); got != resources.Get("discord_reset_done") || !reflect.DeepEqual(model.resetIDs, []int64{123}) {
		t.Fatal("reset did not target channel")
	}
	model.err = errors.New("secret API key")
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: message})
	if len(model.requests) != 3 || sent[len(sent)-1].Content != resources.Get("discord_llm_failed") {
		t.Fatal("reset cooldown or safe error failed")
	}
	message.Author.Bot = true
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: message})
	if len(model.requests) != 3 {
		t.Fatal("bot message triggered LLM")
	}
}

func TestSplitDiscordText(t *testing.T) {
	for _, text := range []string{"", strings.Repeat("я", 4001), strings.Repeat("🙂", 2001), strings.Repeat("a", 1999) + "🙂tail"} {
		parts := splitDiscordText(text)
		if strings.Join(parts, "") != text {
			t.Fatal("text changed")
		}
		for _, part := range parts {
			if len(utf16.Encode([]rune(part))) > 2000 {
				t.Fatal("Discord length exceeded")
			}
		}
	}
}

func TestDiscordHistoryIsSeparateAndPersistent(t *testing.T) {
	cfg := config.Config{LLMHistoryFile: filepath.Join(t.TempDir(), "history.json"), LLMEnabled: true, LLMMaxHistory: 8, LLMBaseURL: "https://example.invalid", LLMModel: "test", LLMTimeout: time.Second}
	discordCfg := discordLLMConfig(cfg)
	if discordCfg.LLMHistoryFile == cfg.LLMHistoryFile {
		t.Fatal("shared history file")
	}
	original := http.DefaultTransport
	http.DefaultTransport = discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	if _, err := llm.New(discordCfg).Ask(context.Background(), llm.Request{ChatID: 123, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if llm.New(cfg).Reset(123) {
		t.Fatal("Discord history leaked into Telegram")
	}
	if !llm.New(discordCfg).Reset(123) {
		t.Fatal("Discord history not persisted")
	}
	if discordLLMConfig(config.Config{}).LLMHistoryFile != "" {
		t.Fatal("enabled persistence without a path")
	}
}
