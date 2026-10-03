package discordbot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
)

func testBanBot(t *testing.T) *Bot {
	t.Helper()
	b, err := New(config.Config{DiscordToken: "test", DiscordPrefix: "!", DiscordOwnerIDs: []string{"1"}, BannedUsersFile: filepath.Join(t.TempDir(), "bans.json")})
	if err != nil {
		t.Fatal(err)
	}
	b.session.State.User = &discordgo.User{ID: "99"}
	return b
}

func TestBanBlocksEveryDiscordMessagePathAndSurvivesRestart(t *testing.T) {
	b := testBanBot(t)
	record := recordMusicUI(t, b)
	owner := musicMessage("!botban <@2>")
	owner.Author.ID = "1"
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: owner})
	if !b.access.IsBanned("2") || len(record.requests) != 1 {
		t.Fatal("owner command did not ban user")
	}
	restarted, err := New(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	b = restarted
	record = recordMusicUI(t, b)
	model := &fakeLanguageModel{enabled: true}
	b.llm = model
	for _, guild := range []string{"", "other-guild"} {
		for _, content := range []string{"hello", "!ping", "!play song", "!gif https://youtu.be/abc", "!llm off", "!botunban 2", "https://youtu.be/abc"} {
			message := musicMessage(content)
			message.GuildID = guild
			b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: message})
		}
	}
	if len(record.requests) != 0 || len(model.requests) != 0 || b.messages.Load() != 0 || b.mediaTotal.Load() != 0 || b.commands.Load() != 0 || len(b.uniqueChannels) != 0 {
		t.Fatal("banned message reached bot services or statistics")
	}
	owner.Content = "!botunban 2"
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: owner})
	if b.access.IsBanned("2") {
		t.Fatal("owner could not unban")
	}
	b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: musicMessage("!ping")})
	if len(record.requests) != 2 {
		t.Fatal("unbanned user cannot use bot")
	}

	if err := os.WriteFile(b.cfg.BannedUsersFile+".discord", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(b.cfg); err == nil {
		t.Fatal("invalid ban file silently disabled restrictions")
	}
}

func TestDiscordBanAuthorizationAndTargets(t *testing.T) {
	for _, tc := range []struct {
		name, author, text, reply string
		forwarded, want           bool
	}{
		{name: "owner numeric", author: "1", text: "!botban 2", want: true},
		{name: "owner mention", author: "1", text: "!botban <@!2>", want: true},
		{name: "owner reply", author: "1", text: "!botban", reply: "2", want: true},
		{name: "server admin", author: "3", text: "!botban 2"},
		{name: "owner protected", author: "1", text: "!botban 1"},
		{name: "bot protected", author: "1", text: "!botban 99"},
		{name: "missing target", author: "1", text: "!botban"},
		{name: "username", author: "1", text: "!botban @someone"},
		{name: "ambiguous", author: "1", text: "!botban 2", reply: "3"},
		{name: "extra target", author: "1", text: "!botban 2 3"},
		{name: "negative", author: "1", text: "!botban -2"},
		{name: "leading zero", author: "1", text: "!botban 02"},
		{name: "forwarded", author: "1", text: "!botban 2", forwarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBanBot(t)
			message := musicMessage(tc.text)
			message.Author.ID = tc.author
			message.Member = &discordgo.Member{Permissions: discordgo.PermissionAdministrator}
			if tc.reply != "" {
				message.ReferencedMessage = &discordgo.Message{Author: &discordgo.User{ID: tc.reply}}
			}
			if tc.forwarded {
				message.MessageReference = &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward}
			}
			b.botBanCommand(b.session, message, true)
			if b.access.IsBanned("2") != tc.want || b.access.IsBanned("1") || b.access.IsBanned("99") || b.access.IsBanned("3") {
				t.Fatal("wrong ban state")
			}
		})
	}
	b := testBanBot(t)
	if err := os.Mkdir(b.cfg.BannedUsersFile+".discord", 0700); err != nil {
		t.Fatal(err)
	}
	message := musicMessage("!botban 2")
	message.Author.ID = "1"
	if reply := b.botBanCommand(b.session, message, true); !strings.Contains(reply, "Не удалось") || b.access.IsBanned("2") {
		t.Fatal("failed persistence acknowledged as successful ban")
	}
}

func TestDiscordBanBlocksSlashButtonsModalsAndAutocomplete(t *testing.T) {
	for _, event := range []*discordgo.InteractionCreate{
		standaloneSlash("ping"), standaloneSlash("gif", slashTextOption("url", "https://youtu.be/abc")),
		standaloneSlash("botunban"), slashMusic("play", map[string]string{"query": "song"}),
		browserComponent("saved", "add"),
		testMusicInteraction(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: "music:modal:ytm"}),
		testMusicInteraction(discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{Name: "play"}),
	} {
		b := testBanBot(t)
		record := recordMusicUI(t, b)
		if err := b.access.SetBanned("2", true); err != nil {
			t.Fatal(err)
		}
		b.handleMusicInteraction(b.session, event)
		callback := record.callback(t, 0)
		if len(record.requests) != 1 || b.commands.Load() != 0 || b.mediaTotal.Load() != 0 || b.music != nil {
			t.Fatal("banned interaction reached a service")
		}
		if event.Type == discordgo.InteractionApplicationCommandAutocomplete {
			if callback["type"] != float64(discordgo.InteractionApplicationCommandAutocompleteResult) {
				t.Fatal("autocomplete did not receive an empty result")
			}
		} else if data := callback["data"].(map[string]any); data["flags"] != float64(discordgo.MessageFlagsEphemeral) || data["content"] != "Доступ к боту заблокирован." {
			t.Fatal("ban rejection was not private")
		}
	}
	b := testBanBot(t)
	recordMusicUI(t, b)
	for _, command := range []string{"botban", "botunban"} {
		event := standaloneSlash(command, &discordgo.ApplicationCommandInteractionDataOption{Name: "user", Type: discordgo.ApplicationCommandOptionUser, Value: "2"})
		event.Member.User.ID = "1"
		b.handleMusicInteraction(b.session, event)
		if b.access.IsBanned("2") != (command == "botban") {
			t.Fatal("slash ban command did not change access")
		}
	}
}

type banWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *banWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestDiscordQueuedMessageRechecksBan(t *testing.T) {
	b := testBanBot(t)
	record := recordMusicUI(t, b)
	b.semaphore = make(chan struct{}, 1)
	b.semaphore <- struct{}{}
	ctx := &banWaitContext{Context: context.Background(), waiting: make(chan struct{})}
	b.ctx = ctx
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleMessageCreate(b.session, &discordgo.MessageCreate{Message: musicMessage("https://youtu.be/abc")})
	}()
	select {
	case <-ctx.waiting:
	case <-time.After(time.Second):
		t.Fatal("message did not reach queue")
	}
	if err := b.access.SetBanned("2", true); err != nil {
		t.Fatal(err)
	}
	<-b.semaphore
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("message did not leave queue")
	}
	if len(record.requests) != 0 || b.messages.Load() != 0 || b.mediaTotal.Load() != 0 {
		t.Fatal("queued banned message reached download")
	}
}
