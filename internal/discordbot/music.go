package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgolink/v4/disgolink"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/disgoorg/snowflake/v2"
	"github.com/mmionya/nyande-bot/resources"
)

const musicQueueLimit = 50

// Only the transport owns Lavalink's client. Queue and voice state are kept
// separately, so Discord callbacks never race with its player event updates.
type musicBackend interface {
	load(context.Context, string) ([]lavalink.Track, error)
	update(context.Context, string, ...disgolink.PlayerUpdateOpt) error
	destroy(context.Context, string) error
	join(string, string) error
	close()
}

type musicGuild struct {
	mu          sync.Mutex
	channel     string
	textChannel string
	voice       lavalink.VoiceState
	current     *lavalink.Track
	queue       []lavalink.Track
}
type musicSearch struct {
	tracks  []lavalink.Track
	expires time.Time
}
type musicService struct {
	backend    musicBackend
	guilds     sync.Map
	searchesMu sync.Mutex
	searches   map[string]musicSearch
	sequence   atomic.Uint64
	ctx        context.Context
	notify     func(string, string)
}

func (m *musicService) guild(id string) *musicGuild {
	value, _ := m.guilds.LoadOrStore(id, &musicGuild{})
	return value.(*musicGuild)
}
func (m *musicService) close() {
	m.guilds.Range(func(key, value any) bool {
		g := value.(*musicGuild)
		g.mu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.stopLocked(ctx, key.(string), g)
		cancel()
		g.mu.Unlock()
		return true
	})
	m.backend.close()
}

func musicIdentifier(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 500 {
		return "", errors.New("invalid query")
	}
	if strings.Contains(query, "://") {
		u, err := url.Parse(query)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
			return "", errors.New("invalid URL")
		}
		host := strings.ToLower(u.Hostname())
		if host != "youtu.be" && host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "soundcloud.com" && !strings.HasSuffix(host, ".soundcloud.com") {
			return "", errors.New("unsupported music host")
		}
		return u.String(), nil
	}
	if strings.HasPrefix(query, "sc:") {
		if strings.TrimSpace(strings.TrimPrefix(query, "sc:")) == "" {
			return "", errors.New("empty SoundCloud query")
		}
		return "scsearch:" + strings.TrimSpace(strings.TrimPrefix(query, "sc:")), nil
	}
	return "ytsearch:" + query, nil
}

func (b *Bot) getMusic(session *discordgo.Session) (*musicService, error) {
	b.musicMu.Lock()
	defer b.musicMu.Unlock()
	if b.music != nil {
		return b.music, nil
	}
	if b.cfg.LavalinkAddress == "" {
		return nil, errors.New("music is not configured")
	}
	if session.State == nil || session.State.User == nil {
		return nil, errors.New("Discord identity is not ready")
	}
	id, err := snowflake.Parse(session.State.User.ID)
	if err != nil {
		return nil, err
	}
	m := &musicService{ctx: b.currentContext(), searches: make(map[string]musicSearch)}
	m.notify = func(channel, text string) {
		_, err := session.ChannelMessageSendComplex(channel, &discordgo.MessageSend{Content: truncateMessage(text, 2000), AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
		if err != nil {
			log.Printf("[music] notification failed: %v", err)
		}
	}
	ready := make(chan struct{})
	var readyOnce sync.Once
	client := disgolink.New(id,
		disgolink.WithListenerFunc(func(e *disgolink.ReadyEvent) {
			first := false
			readyOnce.Do(func() { first = true; close(ready) })
			if !first && !e.Resumed {
				go m.guilds.Range(func(key, _ any) bool { m.voiceClosed(key.(string)); return true })
			}
		}),
		disgolink.WithListenerFunc(func(e *disgolink.PlayerTrackEndEvent) {
			if e.Reason.MayStartNext() {
				go m.trackEnded(e.GuildID.String(), e.Track, e.Reason == lavalink.TrackEndReasonLoadFailed)
			}
		}),
		disgolink.WithListenerFunc(func(e *disgolink.PlayerTrackExceptionEvent) { go m.trackEnded(e.GuildID.String(), e.Track, true) }),
		disgolink.WithListenerFunc(func(e *disgolink.PlayerTrackStuckEvent) { go m.trackEnded(e.GuildID.String(), e.Track, true) }),
		disgolink.WithListenerFunc(func(e *disgolink.PlayerWebSocketClosedEvent) { go m.voiceClosed(e.GuildID.String()) }),
	)
	ctx, cancel := context.WithTimeout(b.currentContext(), 10*time.Second)
	defer cancel()
	node, err := client.AddNode(ctx, disgolink.NodeConfig{Name: "music", Address: b.cfg.LavalinkAddress, Password: b.cfg.LavalinkPassword, Secure: b.cfg.LavalinkSecure})
	if err != nil {
		client.Close()
		return nil, err
	}
	select {
	case <-ready:
	case <-ctx.Done():
		client.Close()
		return nil, ctx.Err()
	}
	m.backend = &lavalinkBackend{client: client, node: node, session: session}
	b.music = m
	return m, nil
}

func (b *Bot) musicCommand(session *discordgo.Session, message *discordgo.Message, command string) string {
	if message.GuildID == "" {
		return resources.Get("music.guild_only")
	}
	m, err := b.getMusic(session)
	if err != nil {
		log.Printf("[music] unavailable: %v", err)
		return resources.Get("music.unavailable")
	}
	fields := strings.Fields(message.Content)
	query := strings.Join(fields[1:], " ")
	ctx, cancel := context.WithTimeout(b.currentContext(), 20*time.Second)
	defer cancel()
	if command == "search" {
		identifier, err := musicIdentifier(query)
		if err != nil {
			return resources.Get("music.query")
		}
		tracks, err := m.backend.load(ctx, identifier)
		if err != nil {
			log.Printf("[music] search failed: %v", err)
			return resources.Get("music.failed")
		}
		if len(tracks) == 0 {
			return resources.Get("music.empty")
		}
		tracks = tracks[:min(len(tracks), 5)]
		m.searchesMu.Lock()
		for key, entry := range m.searches {
			if time.Now().After(entry.expires) {
				delete(m.searches, key)
			}
		}
		m.searches[message.ChannelID+":"+message.Author.ID] = musicSearch{tracks: tracks, expires: time.Now().Add(10 * time.Minute)}
		m.searchesMu.Unlock()
		var text strings.Builder
		for i, track := range tracks {
			fmt.Fprintf(&text, "%d. %s\n", i+1, musicTitle(track))
		}
		text.WriteString(resources.Format("music.choose", map[string]any{"prefix": b.cfg.DiscordPrefix}))
		return truncateMessage(text.String(), 1900)
	}
	g := m.guild(message.GuildID)
	if command == "queue" {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.current == nil {
			return resources.Get("music.queue_empty")
		}
		text := resources.Format("music.current", map[string]any{"title": musicTitle(*g.current)})
		for i, track := range g.queue {
			text += fmt.Sprintf("\n%d. %s", i+1, musicTitle(track))
		}
		return truncateMessage(text, 1900)
	}
	voice, err := session.State.VoiceState(message.GuildID, message.Author.ID)
	if err != nil || voice.ChannelID == "" {
		return resources.Get("music.join_first")
	}
	// Check the cached voice state again after potentially slow search requests.
	var track lavalink.Track
	if command == "play" {
		if number, err := strconv.Atoi(query); err == nil {
			m.searchesMu.Lock()
			entry := m.searches[message.ChannelID+":"+message.Author.ID]
			m.searchesMu.Unlock()
			if time.Now().After(entry.expires) || number < 1 || number > len(entry.tracks) {
				return resources.Get("music.search_expired")
			}
			track = entry.tracks[number-1]
		} else {
			identifier, err := musicIdentifier(query)
			if err != nil {
				return resources.Get("music.query")
			}
			tracks, err := m.backend.load(ctx, identifier)
			if err != nil {
				log.Printf("[music] load failed: %v", err)
				return resources.Get("music.failed")
			}
			if len(tracks) == 0 {
				return resources.Get("music.empty")
			}
			track = tracks[0]
		}
	}
	voice, err = session.State.VoiceState(message.GuildID, message.Author.ID)
	if err != nil || voice.ChannelID == "" {
		return resources.Get("music.join_first")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.channel != "" && g.channel != voice.ChannelID {
		return resources.Get("music.same_channel")
	}
	if command == "play" {
		if len(g.queue) >= musicQueueLimit {
			return resources.Get("music.queue_full")
		}
		if g.channel == "" {
			botID := session.State.User.ID
			permissions, err := session.UserChannelPermissions(botID, voice.ChannelID)
			if err != nil || permissions&(discordgo.PermissionVoiceConnect|discordgo.PermissionVoiceSpeak) != (discordgo.PermissionVoiceConnect|discordgo.PermissionVoiceSpeak) {
				return resources.Get("music.permissions")
			}
			channel, err := session.State.Channel(voice.ChannelID)
			if err != nil || channel.Type != discordgo.ChannelTypeGuildVoice {
				return resources.Get("music.voice_only")
			}
			if err := m.backend.join(message.GuildID, voice.ChannelID); err != nil {
				log.Printf("[music] join failed: %v", err)
				return resources.Get("music.failed")
			}
			g.channel = voice.ChannelID
		}
		g.textChannel = message.ChannelID
		if g.current != nil {
			g.queue = append(g.queue, track)
			return resources.Format("music.queued", map[string]any{"title": musicTitle(track)})
		}
		if err := m.startLocked(ctx, message.GuildID, g, track); err != nil {
			log.Printf("[music] play failed: %v", err)
			_ = m.stopLocked(ctx, message.GuildID, g)
			return resources.Get("music.failed")
		}
		return resources.Format("music.current", map[string]any{"title": musicTitle(track)})
	}
	if g.current == nil {
		return resources.Get("music.queue_empty")
	}
	switch command {
	case "pause", "resume":
		err = m.backend.update(ctx, message.GuildID, disgolink.WithPaused(command == "pause"))
	case "skip":
		err = m.nextLocked(ctx, message.GuildID, g)
	case "stop", "leave":
		err = m.stopLocked(ctx, message.GuildID, g)
	}
	if err != nil {
		log.Printf("[music] %s failed: %v", command, err)
		return resources.Get("music.failed")
	}
	return resources.Get("music." + command)
}

func musicTitle(track lavalink.Track) string {
	return truncateMessage(strings.ReplaceAll(strings.ReplaceAll(track.Info.Title, "\n", " "), "@", "＠"), 100)
}
func (m *musicService) startLocked(ctx context.Context, id string, g *musicGuild, track lavalink.Track) error {
	// A unique token distinguishes stale end events when the same song is replayed.
	tagged, err := track.WithUserData(map[string]uint64{"nyandePlay": m.sequence.Add(1)})
	if err != nil {
		return err
	}
	if err := m.backend.update(ctx, id, disgolink.WithTrack(tagged), disgolink.WithPaused(false)); err != nil {
		return err
	}
	g.current = &tagged
	return nil
}
func (m *musicService) nextLocked(ctx context.Context, id string, g *musicGuild) error {
	if len(g.queue) == 0 {
		return m.stopLocked(ctx, id, g)
	}
	track := g.queue[0]
	if err := m.startLocked(ctx, id, g, track); err != nil {
		return err
	}
	g.queue = g.queue[1:]
	return nil
}
func (m *musicService) stopLocked(ctx context.Context, id string, g *musicGuild) error {
	g.queue = nil
	g.current = nil
	g.channel = ""
	g.voice = lavalink.VoiceState{}
	return errors.Join(m.backend.destroy(ctx, id), m.backend.join(id, ""))
}
func (m *musicService) trackEnded(id string, track lavalink.Track, failed bool) {
	g := m.guild(id)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == nil || musicPlayID(*g.current) != musicPlayID(track) {
		return
	}
	if failed {
		m.notify(g.textChannel, resources.Get("music.track_failed"))
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()
	if err := m.nextLocked(ctx, id, g); err != nil {
		log.Printf("[music] advance failed guild=%s: %v", id, err)
		_ = m.stopLocked(ctx, id, g)
		m.notify(g.textChannel, resources.Get("music.failed"))
	}
}
func (m *musicService) voiceClosed(id string) {
	g := m.guild(id)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.channel == "" {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	_ = m.stopLocked(ctx, id, g)
	m.notify(g.textChannel, resources.Get("music.disconnected"))
}

func (b *Bot) handleMusicVoiceState(session *discordgo.Session, e *discordgo.VoiceStateUpdate) {
	if e.VoiceState == nil || session.State.User == nil || e.UserID != session.State.User.ID {
		return
	}
	b.musicMu.Lock()
	m := b.music
	b.musicMu.Unlock()
	if m == nil {
		return
	}
	g := m.guild(e.GuildID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.channel == "" {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	if e.ChannelID == "" {
		_ = m.stopLocked(ctx, e.GuildID, g)
		return
	}
	g.channel = e.ChannelID
	channel, err := snowflake.Parse(e.ChannelID)
	if err != nil {
		return
	}
	g.voice.ChannelID = channel
	g.voice.SessionID = e.SessionID
	m.updateVoiceLocked(ctx, e.GuildID, g)
}
func (b *Bot) handleMusicVoiceServer(_ *discordgo.Session, e *discordgo.VoiceServerUpdate) {
	b.musicMu.Lock()
	m := b.music
	b.musicMu.Unlock()
	if m == nil {
		return
	}
	g := m.guild(e.GuildID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.channel == "" {
		return
	}
	g.voice.Token = e.Token
	g.voice.Endpoint = e.Endpoint
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	m.updateVoiceLocked(ctx, e.GuildID, g)
}
func (m *musicService) updateVoiceLocked(ctx context.Context, id string, g *musicGuild) {
	if g.voice.ChannelID == 0 || g.voice.SessionID == "" || g.voice.Token == "" || g.voice.Endpoint == "" {
		return
	}
	if err := m.backend.update(ctx, id, disgolink.WithVoice(g.voice)); err != nil {
		log.Printf("[music] voice update failed guild=%s: %v", id, err)
		_ = m.stopLocked(ctx, id, g)
		m.notify(g.textChannel, resources.Get("music.failed"))
	}
}

type lavalinkBackend struct {
	client  *disgolink.Client
	node    *disgolink.Node
	session *discordgo.Session
}

func (l *lavalinkBackend) load(ctx context.Context, query string) ([]lavalink.Track, error) {
	result, err := l.node.Rest.LoadTracks(ctx, query)
	if err != nil {
		return nil, err
	}
	switch data := result.Data.(type) {
	case lavalink.Track:
		return []lavalink.Track{data}, nil
	case lavalink.Search:
		return []lavalink.Track(data), nil
	case lavalink.Playlist:
		return data.Tracks, nil
	case lavalink.Exception:
		return nil, data
	default:
		return nil, nil
	}
}
func (l *lavalinkBackend) update(ctx context.Context, id string, opts ...disgolink.PlayerUpdateOpt) error {
	guild, err := snowflake.Parse(id)
	if err != nil {
		return err
	}
	l.client.Player(guild) // Register for end/exception events; do not read mutable player fields.
	var update lavalink.PlayerUpdate
	for _, opt := range opts {
		opt(&update)
	}
	_, err = l.node.Rest.UpdatePlayer(ctx, guild, update)
	return err
}
func (l *lavalinkBackend) destroy(ctx context.Context, id string) error {
	guild, err := snowflake.Parse(id)
	if err != nil {
		return err
	}
	err = l.node.Rest.DestroyPlayer(ctx, guild)
	l.client.RemovePlayer(guild)
	return err
}
func (l *lavalinkBackend) join(guild, channel string) error {
	return l.session.ChannelVoiceJoinManual(guild, channel, false, true)
}
func (l *lavalinkBackend) close() { l.client.Close() }

// Decode play IDs without relying on JSON object whitespace/order in server events.
func musicPlayID(track lavalink.Track) uint64 {
	var data struct {
		ID uint64 `json:"nyandePlay"`
	}
	_ = json.Unmarshal(track.UserData, &data)
	return data.ID
}
