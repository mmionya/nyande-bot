package discordbot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/mmionya/nyande-bot/resources"
)

type musicUIRequest struct {
	path, method string
	body         map[string]any
}
type musicUIRecorder struct {
	mu       sync.Mutex
	requests []musicUIRequest
}

func recordMusicUI(t *testing.T, b *Bot) *musicUIRecorder {
	t.Helper()
	record := &musicUIRecorder{}
	b.session.Client = &http.Client{Transport: discordRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		record.mu.Lock()
		record.requests = append(record.requests, musicUIRequest{r.URL.Path, r.Method, body})
		record.mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"panel-message","channel_id":"20"}`))}, nil
	})}
	return record
}
func (r *musicUIRecorder) callback(t *testing.T, index int) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var callbacks []map[string]any
	for _, request := range r.requests {
		if strings.HasSuffix(request.path, "/callback") {
			callbacks = append(callbacks, request.body)
		}
	}
	if index < 0 {
		index = len(callbacks) + index
	}
	if index < 0 || index >= len(callbacks) {
		t.Fatalf("callback %d missing", index)
	}
	return callbacks[index]
}
func testMusicInteraction(kind discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "123", AppID: "99", Token: "test-token", Type: kind, GuildID: "1", ChannelID: "20", Member: &discordgo.Member{User: &discordgo.User{ID: "2"}}, Data: data}}
}
func slashMusic(command string, args map[string]string) *discordgo.InteractionCreate {
	var options []*discordgo.ApplicationCommandInteractionDataOption
	for key, value := range args {
		options = append(options, &discordgo.ApplicationCommandInteractionDataOption{Name: key, Type: discordgo.ApplicationCommandOptionString, Value: value})
	}
	return testMusicInteraction(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "nyande", Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: command, Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}}})
}
func browserComponent(id, action string, values ...string) *discordgo.InteractionCreate {
	i := testMusicInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "music:search:" + id + ":" + action, Values: values})
	i.Message = &discordgo.Message{ID: "browser", ChannelID: "20"}
	return i
}
func onlyMusicBrowser(t *testing.T, b *Bot) *musicBrowser {
	t.Helper()
	b.musicUI.mu.Lock()
	defer b.musicUI.mu.Unlock()
	for _, v := range b.musicUI.browsers {
		return v
	}
	t.Fatal("browser missing")
	return nil
}

func TestMusicSlashOpensModalAndDefersSearch(t *testing.T) {
	b, _ := testMusicBot(t)
	record := recordMusicUI(t, b)
	b.handleMusicInteraction(b.session, slashMusic("search", map[string]string{"source": "ytm"}))
	callback := record.callback(t, 0)
	if callback["type"] != float64(discordgo.InteractionResponseModal) {
		t.Fatalf("not a modal: %#v", callback)
	}
	if callback["data"].(map[string]any)["custom_id"] != "music:modal:ytm" {
		t.Fatal("source lost in modal")
	}
	submitted := testMusicInteraction(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: "music:modal:ytm", Components: []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: "query", Value: "a song"}}}}})
	b.handleMusicInteraction(b.session, submitted)
	callback = record.callback(t, 1)
	if callback["type"] != float64(discordgo.InteractionResponseDeferredChannelMessageWithSource) || callback["data"].(map[string]any)["flags"] != float64(discordgo.MessageFlagsEphemeral) {
		t.Fatal("search was not deferred privately")
	}
	browser := onlyMusicBrowser(t, b)
	if browser.source != "ytm" || browser.query != "a song" || len(browser.tracks) != 2 {
		t.Fatal("wrong modal results")
	}
	if err := b.registerMusicCommands(); err != nil {
		t.Fatal(err)
	}
	schema := musicApplicationCommand()
	found := false
	for _, option := range schema.Options {
		if option.Name == "search" {
			for _, arg := range option.Options {
				if arg.Name == "source" {
					found = len(arg.Choices) == 4
				}
			}
		}
	}
	if !found {
		t.Fatal("slash sources are missing")
	}
}

func TestMusicBrowserMultiSelectPagesAndIdempotentAdd(t *testing.T) {
	b, backend := testMusicBot(t)
	record := recordMusicUI(t, b)
	backend.tracks = nil
	for i := 0; i < 12; i++ {
		backend.tracks = append(backend.tracks, musicTrack(fmt.Sprintf("song-%d", i)))
	}
	b.handleMusicInteraction(b.session, slashMusic("search", map[string]string{"query": "songs"}))
	browser := onlyMusicBrowser(t, b)
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "select:0", "2", "0"))
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "page:1"))
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "select:1", "11"))
	// A selection from a stale page must not overwrite the current selection.
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "select:0", "1"))
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "add"))
	g := b.music.guild("1")
	if g.current == nil || g.current.Info.Title != "song-0" || len(g.queue) != 2 || g.queue[0].Info.Title != "song-2" || g.queue[1].Info.Title != "song-11" {
		t.Fatal("selection order or enqueue is wrong")
	}
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "add"))
	if len(g.queue) != 2 || len(backend.updates) != 1 {
		t.Fatal("repeated add duplicated tracks")
	}
	if !browser.consumed {
		t.Fatal("successful selection remains active")
	}
	callback := record.callback(t, 1)
	if callback["type"] != float64(discordgo.InteractionResponseDeferredMessageUpdate) {
		t.Fatal("component was not acknowledged before processing")
	}
	b.musicUI.panelMu.Lock()
	panel, exists := b.musicUI.panels["1"]
	b.musicUI.panelMu.Unlock()
	if !exists || panel.id != "panel-message" {
		t.Fatal("public player card was not created")
	}
}

func TestMusicBrowserRejectsForeignExpiredAndInvalidSelection(t *testing.T) {
	b, backend := testMusicBot(t)
	record := recordMusicUI(t, b)
	b.handleMusicInteraction(b.session, slashMusic("search", map[string]string{"query": "songs"}))
	browser := onlyMusicBrowser(t, b)
	foreign := browserComponent(browser.id, "select:0", "0")
	foreign.Member.User.ID = "3"
	b.handleMusicInteraction(b.session, foreign)
	if len(browser.selected) != 0 {
		t.Fatal("foreign user modified selection")
	}
	if record.callback(t, -1)["data"].(map[string]any)["flags"] != float64(discordgo.MessageFlagsEphemeral) {
		t.Fatal("rejection was public")
	}
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "select:0", "999"))
	if len(browser.selected) != 0 {
		t.Fatal("invalid selected index accepted")
	}
	browser.expires = time.Now().Add(-time.Second)
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "add"))
	if len(backend.updates) != 0 {
		t.Fatal("expired browser started playback")
	}
}

func TestMusicBatchCapacityAndFailureAreAtomic(t *testing.T) {
	b, backend := testMusicBot(t)
	b.musicCommand(b.session, musicMessage("!play song"), "play")
	g := b.music.guild("1")
	g.queue = make([]lavalink.Track, 49)
	_, ok := b.enqueueMusicTracks(context.Background(), b.session, musicMessage("!play"), b.music, backend.tracks)
	if ok || len(g.queue) != 49 {
		t.Fatal("overfull batch was partially added")
	}
	g.queue = nil
	before := g.current
	backend.updateErr = fmt.Errorf("backend failed")
	b.musicCommand(b.session, musicMessage("!volume 20"), "volume")
	if g.volume != 50 {
		t.Fatal("failed volume update changed state")
	}
	b.musicCommand(b.session, musicMessage("!pause"), "pause")
	if g.paused || g.current != before {
		t.Fatal("failed pause changed state")
	}
}

func TestMusicControlsRepeatMoveAndProgress(t *testing.T) {
	b, _ := testMusicBot(t)
	b.musicCommand(b.session, musicMessage("!play song"), "play")
	g := b.music.guild("1")
	g.current.Info.Length = 180000
	g.position = 2000
	g.positionAt = time.Now().Add(-3 * time.Second)
	if got := musicPosition(g, time.Now()); got < 5000 || got > 5500 {
		t.Fatalf("position %d", got)
	}
	b.musicCommand(b.session, musicMessage("!pause"), "pause")
	paused := g.position
	if musicPosition(g, time.Now().Add(time.Minute)) != paused {
		t.Fatal("paused position advanced")
	}
	b.musicCommand(b.session, musicMessage("!repeat track"), "repeat")
	old := *g.current
	b.music.trackEnded("1", old, false)
	if g.current == nil || musicPlayID(*g.current) == musicPlayID(old) {
		t.Fatal("repeat did not restart with a new play ID")
	}
	g.queue = []lavalink.Track{musicTrack("a"), musicTrack("b"), musicTrack("c")}
	b.musicCommand(b.session, musicMessage("!move 1 3"), "move")
	if !reflect.DeepEqual([]string{g.queue[0].Encoded, g.queue[1].Encoded, g.queue[2].Encoded}, []string{"b", "c", "a"}) {
		t.Fatal("move failed")
	}
	b.musicCommand(b.session, musicMessage("!move 3 1"), "move")
	if g.queue[0].Encoded != "a" {
		t.Fatal("reverse move failed")
	}
	b.musicCommand(b.session, musicMessage("!repeat queue"), "repeat")
	current := *g.current
	b.music.trackEnded("1", current, false)
	if g.current.Encoded != "a" || len(g.queue) != 3 || g.queue[2].Encoded != current.Encoded {
		t.Fatal("queue repeat lost track")
	}
	b.musicCommand(b.session, musicMessage("!clear"), "clear")
	if g.current == nil || len(g.queue) != 0 {
		t.Fatal("clear stopped current track")
	}
	b.musicCommand(b.session, musicMessage("!volume 100"), "volume")
	b.musicCommand(b.session, musicMessage("!volup"), "volup")
	if g.volume != 100 {
		t.Fatal("volume exceeded limit")
	}
}

func TestMusicPlayerButtonsCheckVoiceAndDisableAfterStop(t *testing.T) {
	b, backend := testMusicBot(t)
	recordMusicUI(t, b)
	b.musicCommand(b.session, musicMessage("!play song"), "play")
	if err := b.refreshMusicPanel(b.session, "1", "20", true); err != nil {
		t.Fatal(err)
	}
	panel := b.musicUI.panels["1"]
	button := func(action string) *discordgo.InteractionCreate {
		i := testMusicInteraction(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "music:panel:" + panel.nonce + ":" + action})
		i.Message = &discordgo.Message{ID: panel.id, ChannelID: "20"}
		return i
	}
	foreign := button("stop")
	foreign.Member.User.ID = "3"
	b.handleMusicInteraction(b.session, foreign)
	if backend.destroyed != 0 {
		t.Fatal("user outside voice stopped playback")
	}
	b.handleMusicInteraction(b.session, button("toggle"))
	g := b.music.guild("1")
	if !g.paused {
		t.Fatal("pause button did not pause")
	}
	b.handleMusicInteraction(b.session, button("stop"))
	if g.current != nil {
		t.Fatal("stop failed")
	}
	_, components := musicPanelView(g, panel.nonce)
	for _, row := range components {
		for _, component := range row.(discordgo.ActionsRow).Components {
			if !component.(discordgo.Button).Disabled {
				t.Fatal("stopped panel still has enabled buttons")
			}
		}
	}
	count := backend.destroyed
	b.handleMusicInteraction(b.session, button("stop"))
	if backend.destroyed != count {
		t.Fatal("stale panel mutated player")
	}
}

func TestMusicBrowserRenderFitsDiscordLimits(t *testing.T) {
	browser := &musicBrowser{id: "id", source: "yt", query: strings.Repeat("q", 500), selected: map[int]bool{0: true}}
	for i := 0; i < 10; i++ {
		track := musicTrack(strings.Repeat("🙂", 200))
		track.Info.Author = strings.Repeat("a", 120)
		track.Info.Length = 123000
		browser.tracks = append(browser.tracks, track)
	}
	view := browser.render()
	// Emoji titles must fit Discord's UTF-16 limits as well as rune limits.
	for _, row := range view.Components {
		for _, component := range row.(discordgo.ActionsRow).Components {
			if menu, ok := component.(discordgo.SelectMenu); ok {
				if len(menu.Options) > 25 {
					t.Fatal("too many options")
				}
				for _, option := range menu.Options {
					if len(utf16.Encode([]rune(option.Label))) > 100 {
						t.Fatal("label too long")
					}
				}
			}
		}
	}
	if view.Embeds[0].Description == "" || view.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu).Options[0].Default != true {
		t.Fatal("missing selection display")
	}
	if got := musicDuration(123000); got != "2:03" {
		t.Fatal(got)
	}
	if resources.Get("music.ui_help") == "" {
		t.Fatal("help missing")
	}
}

func TestMusicConcurrentAddIsIdempotent(t *testing.T) {
	b, backend := testMusicBot(t)
	recordMusicUI(t, b)
	b.handleMusicInteraction(b.session, slashMusic("search", map[string]string{"query": "songs"}))
	browser := onlyMusicBrowser(t, b)
	b.handleMusicInteraction(b.session, browserComponent(browser.id, "select:0", "0", "1"))
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.handleMusicInteraction(b.session, browserComponent(browser.id, "add")) }()
	}
	wg.Wait()
	g := b.music.guild("1")
	if len(g.queue) != 1 || len(backend.updates) != 1 {
		t.Fatal("concurrent retries duplicated selected tracks")
	}
}
