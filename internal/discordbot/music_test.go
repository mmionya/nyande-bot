package discordbot

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgolink/v4/disgolink"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/resources"
)

type fakeMusicBackend struct {
	tracks    []lavalink.Track
	updates   []lavalink.PlayerUpdate
	joins     []string
	destroyed int
	updateErr error
}

func (f *fakeMusicBackend) load(context.Context, string) ([]lavalink.Track, error) {
	return f.tracks, nil
}
func (f *fakeMusicBackend) update(_ context.Context, _ string, opts ...disgolink.PlayerUpdateOpt) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	var update lavalink.PlayerUpdate
	for _, opt := range opts {
		opt(&update)
	}
	f.updates = append(f.updates, update)
	return nil
}
func (f *fakeMusicBackend) destroy(context.Context, string) error { f.destroyed++; return nil }
func (f *fakeMusicBackend) join(_, channel string) error {
	f.joins = append(f.joins, channel)
	return nil
}
func (f *fakeMusicBackend) close() {}
func musicTrack(name string) lavalink.Track {
	return lavalink.Track{Encoded: name, Info: lavalink.TrackInfo{Title: name}}
}
func testMusicBot(t *testing.T) (*Bot, *fakeMusicBackend) {
	t.Helper()
	backend := &fakeMusicBackend{tracks: []lavalink.Track{musicTrack("first"), musicTrack("second")}}
	m := &musicService{backend: backend, ctx: context.Background(), searches: make(map[string]musicSearch), notify: func(string, string) {}}
	session, _ := discordgo.New("Bot test")
	session.State.User = &discordgo.User{ID: "99"}
	session.Client = &http.Client{Transport: discordRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected HTTP %s", r.URL.Path)
		return nil, errors.New("unexpected HTTP")
	})}
	err := session.State.GuildAdd(&discordgo.Guild{ID: "1", Roles: []*discordgo.Role{{ID: "1", Permissions: discordgo.PermissionVoiceConnect | discordgo.PermissionVoiceSpeak}},
		Members:     []*discordgo.Member{{User: session.State.User}},
		Channels:    []*discordgo.Channel{{ID: "10", GuildID: "1", Type: discordgo.ChannelTypeGuildVoice}},
		VoiceStates: []*discordgo.VoiceState{{GuildID: "1", UserID: "2", ChannelID: "10"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{music: m, session: session, cfg: config.Config{DiscordPrefix: "!"}, llm: &fakeLanguageModel{enabled: false}}
	return b, backend
}
func musicMessage(text string) *discordgo.Message {
	return &discordgo.Message{ID: "5", GuildID: "1", ChannelID: "20", Author: &discordgo.User{ID: "2"}, Content: text}
}
func TestMusicSearchPlayAndQueue(t *testing.T) {
	b, backend := testMusicBot(t)
	if result := b.musicCommand(b.session, musicMessage("!search songs"), "search"); result == resources.Get("music.failed") {
		t.Fatal(result)
	}
	result := b.musicCommand(b.session, musicMessage("!play 2"), "play")
	if len(backend.updates) != 1 {
		t.Fatalf("not played: %s", result)
	}
	g := b.music.guild("1")
	if g.current.Info.Title != "second" || g.channel != "10" {
		t.Fatal("wrong search selection or channel")
	}
	b.musicCommand(b.session, musicMessage("!play songs"), "play")
	if len(g.queue) != 1 || len(backend.updates) != 1 {
		t.Fatal("enqueue replaced current track")
	}
	first := *g.current
	b.music.trackEnded("1", first, false)
	if g.current.Info.Title != "first" || len(g.queue) != 0 {
		t.Fatal("end did not advance queue")
	}
	b.music.trackEnded("1", first, false)
	if g.current == nil || g.current.Info.Title != "first" {
		t.Fatal("stale event affected new track")
	}
	b.musicCommand(b.session, musicMessage("!pause"), "pause")
	if backend.updates[len(backend.updates)-1].Paused == nil || !*backend.updates[len(backend.updates)-1].Paused {
		t.Fatal("not paused")
	}
	b.musicCommand(b.session, musicMessage("!resume"), "resume")
	if *backend.updates[len(backend.updates)-1].Paused {
		t.Fatal("not resumed")
	}
	b.musicCommand(b.session, musicMessage("!stop"), "stop")
	if g.current != nil || g.channel != "" || len(g.queue) != 0 || backend.joins[len(backend.joins)-1] != "" {
		t.Fatal("stop left state or voice connection")
	}
}
func TestMusicAccessAndLimits(t *testing.T) {
	b, backend := testMusicBot(t)
	m := musicMessage("!play songs")
	m.GuildID = ""
	if got := b.musicCommand(b.session, m, "play"); got != resources.Get("music.guild_only") {
		t.Fatal(got)
	}
	m = musicMessage("!play songs")
	m.Author.ID = "3"
	if got := b.musicCommand(b.session, m, "play"); got != resources.Get("music.join_first") {
		t.Fatal(got)
	}
	g := b.music.guild("1")
	g.channel = "other"
	track := musicTrack("old")
	g.current = &track
	if got := b.musicCommand(b.session, musicMessage("!skip"), "skip"); got != resources.Get("music.same_channel") {
		t.Fatal(got)
	}
	if len(backend.updates) != 0 {
		t.Fatal("unauthorized command updated player")
	}
	g.channel = "10"
	g.queue = make([]lavalink.Track, musicQueueLimit)
	if got := b.musicCommand(b.session, musicMessage("!play songs"), "play"); got != resources.Get("music.queue_full") {
		t.Fatal(got)
	}
	b.music.searches["20:2"] = musicSearch{tracks: backend.tracks, expires: time.Now().Add(-time.Second)}
	if got := b.musicCommand(b.session, musicMessage("!play 1"), "play"); got != resources.Get("music.search_expired") {
		t.Fatal(got)
	}
	if b.music.guild("other").current != nil {
		t.Fatal("guilds share a queue")
	}
}
func TestMusicFailureAndEndCleanup(t *testing.T) {
	b, backend := testMusicBot(t)
	backend.updateErr = errors.New("offline")
	b.musicCommand(b.session, musicMessage("!play songs"), "play")
	g := b.music.guild("1")
	if g.current != nil || g.channel != "" {
		t.Fatal("failed start retained state")
	}
	backend.updateErr = nil
	b.musicCommand(b.session, musicMessage("!play songs"), "play")
	b.music.trackEnded("1", *g.current, false)
	if g.current != nil || g.channel != "" {
		t.Fatal("empty queue did not disconnect")
	}
}
func TestMusicVoicePayloadIncludesDAVEChannel(t *testing.T) {
	b, backend := testMusicBot(t)
	g := b.music.guild("1")
	g.channel = "10"
	// Either ordering of the two Discord voice events must work.
	b.handleMusicVoiceServer(b.session, &discordgo.VoiceServerUpdate{GuildID: "1", Token: "voice-token", Endpoint: "voice.example"})
	if len(backend.updates) != 0 {
		t.Fatal("sent incomplete voice state")
	}
	b.handleMusicVoiceState(b.session, &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{GuildID: "1", UserID: "99", ChannelID: "10", SessionID: "session"}})
	if len(backend.updates) != 1 {
		t.Fatal("voice state was not forwarded")
	}
	voice := backend.updates[0].Voice
	if voice.ChannelID.String() != "10" || voice.SessionID != "session" || voice.Token != "voice-token" {
		t.Fatalf("invalid voice payload: %+v", voice)
	}
	b.handleMusicVoiceState(b.session, &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{GuildID: "1", UserID: "99", ChannelID: ""}})
	if g.channel != "" || g.current != nil {
		t.Fatal("kick did not clear state")
	}
}
func TestMusicIdentifier(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"my song", "ytsearch:my song"},
		{"yt: my song", "ytsearch:my song"},
		{"ytm: my song", "ytmsearch:my song"},
		{"YTM: my song", "ytmsearch:my song"},
		{"bc: my song", "bcsearch:my song"},
		{"https://music.youtube.com/watch?v=abc", "https://music.youtube.com/watch?v=abc"},
		{"https://artist.bandcamp.com/track/song", "https://artist.bandcamp.com/track/song"}, {"sc: my song", "scsearch:my song"}, {"https://youtu.be/abc", "https://youtu.be/abc"},
	} {
		got, err := musicIdentifier(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q => %q, %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"", "sc:", "ytm:  ", "bc:", "yt:", "https://bandcamp.com.evil.example/track/a", "https://bandcamp.com@evil.example/a", "file:///etc/passwd", "http://youtube.com/a", "https://127.0.0.1/a", "https://youtube.com.evil.example/a", "https://user@youtube.com/a", "https://youtube.com:123/a"} {
		if _, err := musicIdentifier(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
