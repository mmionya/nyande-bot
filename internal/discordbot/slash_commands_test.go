package discordbot

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/resources"
)

func standaloneSlash(name string, options ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.InteractionCreate {
	return testMusicInteraction(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: name, Options: options})
}
func slashTextOption(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value}
}
func lastSlashText(t *testing.T, r *musicUIRecorder) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for n := len(r.requests) - 1; n >= 0; n-- {
		if text, ok := r.requests[n].body["content"].(string); ok {
			return text
		}
	}
	t.Fatal("missing response content")
	return ""
}
func TestStandaloneSlashRegistration(t *testing.T) {
	b, _ := testMusicBot(t)
	record := recordMusicUI(t, b)
	if err := b.registerDiscordCommands(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range record.requests {
		if r.method != "POST" || !strings.HasSuffix(r.path, "/commands") {
			t.Fatalf("unexpected registration: %s %s", r.method, r.path)
		}
		name := r.body["name"].(string)
		if seen[name] {
			t.Fatal("duplicate command", name)
		}
		seen[name] = true
	}
	for _, name := range []string{"nyande", "search", "play", "player", "queue", "skip", "pause", "resume", "stop", "shuffle", "clear", "move", "volume", "repeat", "help", "ping", "stats", "reset", "llm", "gif"} {
		if !seen[name] {
			t.Fatal("command absent from picker:", name)
		}
	}
}
func TestStandaloneMusicRoutesToExistingBrowser(t *testing.T) {
	b, _ := testMusicBot(t)
	record := recordMusicUI(t, b)
	event := standaloneSlash("search", slashTextOption("source", "sc"))
	b.handleMusicInteraction(b.session, event)
	if record.callback(t, 0)["type"] != float64(discordgo.InteractionResponseModal) {
		t.Fatal("search did not open modal")
	}
	if event.ApplicationCommandData().Name != "search" {
		t.Fatal("mutated shared interaction")
	}
	b.handleMusicInteraction(b.session, standaloneSlash("search", slashTextOption("query", "song"), slashTextOption("source", "ytm")))
	browser := onlyMusicBrowser(t, b)
	if browser.source != "ytm" || browser.query != "song" {
		t.Fatal("search parameters lost")
	}
	b.handleMusicInteraction(b.session, standaloneSlash("queue"))
	if lastSlashText(t, record) != resources.Get("music.queue_empty") {
		t.Fatal("queue route failed")
	}
	dm := standaloneSlash("play", slashTextOption("query", "song"))
	dm.GuildID = ""
	b.handleMusicInteraction(b.session, dm)
	if record.callback(t, -1)["data"].(map[string]any)["content"] != resources.Get("music.guild_only") {
		t.Fatal("music allowed in DM")
	}
}
func TestGeneralSlashPrivateResponsesAndLLMPermissions(t *testing.T) {
	b, _ := testMusicBot(t)
	record := recordMusicUI(t, b)
	for _, command := range []string{"help", "ping", "stats", "reset", "llm"} {
		event := standaloneSlash(command)
		event.GuildID = ""
		b.handleMusicInteraction(b.session, event)
		callback := record.callback(t, -1)
		if callback["type"] != float64(discordgo.InteractionResponseDeferredChannelMessageWithSource) || callback["data"].(map[string]any)["flags"] != float64(discordgo.MessageFlagsEphemeral) {
			t.Fatal("command did not acknowledge privately", command)
		}
	}
	settings, err := loadLLMSettings(filepath.Join(t.TempDir(), "llm.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.llmSettings = settings
	guild, err := b.session.State.Guild("1")
	if err != nil {
		t.Fatal(err)
	}
	guild.Channels = append(guild.Channels, &discordgo.Channel{ID: "20", GuildID: "1"})
	guild.Members = append(guild.Members, &discordgo.Member{User: &discordgo.User{ID: "2"}})
	if err = b.session.State.GuildAdd(guild); err != nil {
		t.Fatal(err)
	}
	b.handleMusicInteraction(b.session, standaloneSlash("llm", slashTextOption("mode", "off")))
	if settings.isDisabled("guild:1") || lastSlashText(t, record) != resources.Get("discord_llm_admin_only") {
		t.Fatal("ordinary member disabled LLM")
	}
	guild.Roles[0].Permissions |= discordgo.PermissionManageServer
	if err = b.session.State.GuildAdd(guild); err != nil {
		t.Fatal(err)
	}
	b.handleMusicInteraction(b.session, standaloneSlash("llm", slashTextOption("mode", "off")))
	if !settings.isDisabled("guild:1") {
		t.Fatal("manager could not disable LLM")
	}
}
func TestSlashGIFMissingAttachmentAndUnknownCommands(t *testing.T) {
	b, _ := testMusicBot(t)
	record := recordMusicUI(t, b)
	event := standaloneSlash("gif", &discordgo.ApplicationCommandInteractionDataOption{Name: "video", Type: discordgo.ApplicationCommandOptionAttachment, Value: "123"})
	data := event.ApplicationCommandData()
	data.Resolved = &discordgo.ApplicationCommandInteractionDataResolved{Attachments: map[string]*discordgo.MessageAttachment{"123": {URL: "https://example.com/not-video.txt", ContentType: "text/plain"}}}
	event.Data = data
	b.handleMusicInteraction(b.session, event)
	if !strings.Contains(lastSlashText(t, record), "video") {
		t.Fatal("missing attachment not explained")
	}
	before := len(record.requests)
	b.handleMusicInteraction(b.session, standaloneSlash("foreign-command"))
	if len(record.requests) != before {
		t.Fatal("handled another command")
	}
}
