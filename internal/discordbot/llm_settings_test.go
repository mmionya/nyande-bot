package discordbot

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
)

func TestLLMSettingsCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	b, err := New(config.Config{DiscordToken: "test", DiscordPrefix: "?", DiscordLLMSettingsFile: path, LLMTriggerWords: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeLanguageModel{enabled: true}
	b.llm = model
	b.session.State.User = &discordgo.User{ID: "99"}
	err = b.session.State.GuildAdd(&discordgo.Guild{ID: "1", OwnerID: "owner",
		Roles:    []*discordgo.Role{{ID: "1"}, {ID: "manager", Permissions: discordgo.PermissionManageServer}},
		Members:  []*discordgo.Member{{User: &discordgo.User{ID: "admin"}, Roles: []string{"manager"}}, {User: &discordgo.User{ID: "member"}}},
		Channels: []*discordgo.Channel{{ID: "123", GuildID: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var replies []string
	b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/messages") {
			t.Fatalf("unexpected request %s", req.URL.Path)
		}
		var payload discordgo.MessageSend
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, payload.Content)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"reply"}`))}, nil
	})}
	command := func(user, text, guild string) {
		m := &discordgo.Message{ID: "10", ChannelID: "123", GuildID: guild, Author: &discordgo.User{ID: user}, Content: text}
		b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: m})
	}
	command("member", "?llm off", "1")
	if b.llmSettings.isDisabled("guild:1") {
		t.Fatal("ordinary member disabled LLM")
	}
	command("admin", "?llm off", "1")
	if !b.llmSettings.isDisabled("guild:1") {
		t.Fatal("manager failed to disable LLM")
	}
	for _, m := range []discordgo.Message{
		{GuildID: "1", ChannelID: "456", Content: "hello"},
		{GuildID: "1", ChannelID: "456", Content: "<@99>", Mentions: []*discordgo.User{{ID: "99"}}},
		{GuildID: "1", ChannelID: "456", Content: "reply", ReferencedMessage: &discordgo.Message{Author: &discordgo.User{ID: "99"}}},
	} {
		b.handleLLM(b.session, &m)
		if b.shouldAnswerLLM(b.session, &m) {
			t.Fatal("disabled guild still triggers LLM")
		}
	}
	if len(model.requests) != 0 {
		t.Fatal("disabled LLM was called")
	}
	if !b.shouldAnswerLLM(b.session, &discordgo.Message{GuildID: "2", Content: "hello"}) {
		t.Fatal("other guild affected")
	}
	if !b.shouldAnswerLLM(b.session, &discordgo.Message{ChannelID: "123", Content: "hello"}) {
		t.Fatal("DM affected by guild setting")
	}
	restored, err := loadLLMSettings(path)
	if err != nil || !restored.isDisabled("guild:1") {
		t.Fatalf("setting not persisted: %v", err)
	}
	command("admin", "?llm", "1")
	command("admin", "?llm invalid", "1")
	if !b.llmSettings.isDisabled("guild:1") {
		t.Fatal("status or invalid command changed settings")
	}
	command("admin", "?ping", "1")
	command("admin", "?llm on", "1")
	if b.llmSettings.isDisabled("guild:1") {
		t.Fatal("reenabling failed")
	}
	command("member", "?llm off", "")
	if !b.llmSettings.isDisabled("dm:123") {
		t.Fatal("DM disabling failed")
	}
	model.enabled = false
	command("member", "?llm on", "")
	if !b.llmSettings.isDisabled("dm:123") {
		t.Fatal("global LLM setting bypassed")
	}
	if len(replies) != 8 {
		t.Fatalf("got %d command replies", len(replies))
	}
}

func TestLLMSettingsFailedSavePreservesState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := loadLLMSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setDisabled("guild:1", true); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(blocker, "settings.json")
	if err := s.setDisabled("guild:1", false); err == nil {
		t.Fatal("expected save failure")
	}
	if !s.isDisabled("guild:1") {
		t.Fatal("save failure changed state")
	}
}
