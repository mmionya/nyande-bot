package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
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
const musicPlayTimeout = 45 * time.Second

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
	mu             sync.Mutex
	channel        string
	textChannel    string
	voice          lavalink.VoiceState
	current        *lavalink.Track
	queue          []lavalink.Track
	volume         int
	paused         bool
	repeat         string
	position       lavalink.Duration
	positionAt     time.Time
	positionSource func() lavalink.Duration
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
	value, _ := m.guilds.LoadOrStore(id, &musicGuild{volume: 50})
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
		if host != "youtu.be" && host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "soundcloud.com" && !strings.HasSuffix(host, ".soundcloud.com") && host != "bandcamp.com" && !strings.HasSuffix(host, ".bandcamp.com") {
			return "", errors.New("unsupported music host")
		}
		return u.String(), nil
	}
	if source, text, ok := strings.Cut(query, ":"); ok {
		prefixes := map[string]string{
			"yt": "ytsearch:", "ytm": "ytmsearch:",
			"sc": "scsearch:", "bc": "bcsearch:",
		}
		if prefix, exists := prefixes[strings.ToLower(source)]; exists {
			text = strings.TrimSpace(text)
			if text == "" {
				return "", errors.New("empty music search query")
			}
			return prefix + text, nil
		}
	}
	return "ytsearch:" + query, nil
}

func (b *Bot) getMusic(session *discordgo.Session) (*musicService, error) {
	b.musicMu.Lock()
	defer b.musicMu.Unlock()
	if b.music != nil {
		return b.music, nil
	}
	if b.cfg.MusicBackend == "lavalink" && b.cfg.LavalinkAddress == "" {
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
	if b.cfg.MusicBackend == "" || b.cfg.MusicBackend == "direct" {
		backend, err := newDirectMusicBackend(m, session, b.cfg)
		if err != nil {
			return nil, err
		}
		m.backend = backend
		b.music = m
		return m, nil
	}
	if b.cfg.MusicBackend != "lavalink" {
		return nil, errors.New("unknown DISCORD_MUSIC_BACKEND")
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
		disgolink.WithListenerFunc(func(e *disgolink.PlayerUpdateEvent) {
			g := m.guild(e.GuildID.String())
			g.mu.Lock()
			if g.current != nil {
				g.position = e.State.Position
				g.positionAt = time.Now()
			}
			g.mu.Unlock()
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
	ctx, cancel := context.WithTimeout(b.currentContext(), musicPlayTimeout)
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
			var tracks []lavalink.Track
			if direct, ok := m.backend.(*directMusicBackend); ok {
				tracks, err = direct.extractor.loadLimit(ctx, identifier, 1)
			} else {
				tracks, err = m.backend.load(ctx, identifier)
			}
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
	if command == "play" {
		result, _ := b.enqueueMusicTracks(ctx, session, message, m, []lavalink.Track{track})
		return result
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

	if g.current == nil {
		return resources.Get("music.queue_empty")
	}
	switch command {
	case "pause", "resume", "toggle":
		paused := command == "pause" || (command == "toggle" && !g.paused)
		err = m.backend.update(ctx, message.GuildID, disgolink.WithPaused(paused))
		if err == nil {
			g.position = musicPosition(g, time.Now())
			g.positionAt = time.Now()
			g.paused = paused
		}
	case "volume", "volup", "voldown":
		value, parseErr := strconv.Atoi(query)
		if command == "volup" {
			value = min(100, g.volume+10)
			parseErr = nil
		}
		if command == "voldown" {
			value = max(0, g.volume-10)
			parseErr = nil
		}
		if parseErr != nil || value < 0 || value > 100 {
			return resources.Get("music.volume_usage")
		}
		err = m.backend.update(ctx, message.GuildID, disgolink.WithVolume(value))
		if err == nil {
			g.volume = value
		}
	case "repeat":
		mode := strings.ToLower(query)
		if mode == "" {
			mode = map[string]string{"": "track", "off": "track", "track": "queue", "queue": "off"}[g.repeat]
		}
		if mode != "off" && mode != "track" && mode != "queue" {
			return resources.Get("music.repeat_usage")
		}
		g.repeat = mode
	case "shuffle":
		rand.Shuffle(len(g.queue), func(i, j int) { g.queue[i], g.queue[j] = g.queue[j], g.queue[i] })
	case "clear":
		g.queue = nil
	case "move":
		positions := strings.Fields(query)
		if len(positions) != 2 {
			return resources.Get("music.move_usage")
		}
		from, err1 := strconv.Atoi(positions[0])
		to, err2 := strconv.Atoi(positions[1])
		if err1 != nil || err2 != nil || from < 1 || from > len(g.queue) || to < 1 || to > len(g.queue) {
			return resources.Get("music.move_usage")
		}
		moveMusicTrack(g.queue, from-1, to-1)
	case "skip":
		err = m.nextLocked(ctx, message.GuildID, g)
	case "stop", "leave":
		err = m.stopLocked(ctx, message.GuildID, g)
	}
	if err != nil {
		log.Printf("[music] %s failed: %v", command, err)
		return resources.Get("music.failed")
	}
	if command == "volume" || command == "volup" || command == "voldown" {
		return resources.Format("music.volume_set", map[string]any{"volume": g.volume})
	}
	if command == "repeat" {
		return resources.Format("music.repeat_set", map[string]any{"mode": g.repeat})
	}
	if command == "toggle" {
		if g.paused {
			command = "pause"
		} else {
			command = "resume"
		}
	}
	return resources.Get("music." + command)
}

func (b *Bot) enqueueMusicTracks(ctx context.Context, session *discordgo.Session, message *discordgo.Message, m *musicService, tracks []lavalink.Track) (string, bool) {
	if len(tracks) == 0 {
		return resources.Get("music.empty"), false
	}
	voice, err := session.State.VoiceState(message.GuildID, message.Author.ID)
	if err != nil || voice.ChannelID == "" {
		return resources.Get("music.join_first"), false
	}
	g := m.guild(message.GuildID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.channel != "" && g.channel != voice.ChannelID {
		return resources.Get("music.same_channel"), false
	}
	pending := len(g.queue) + len(tracks)
	if g.current == nil {
		pending--
	}
	if pending > musicQueueLimit {
		return resources.Get("music.queue_full"), false
	}
	if g.channel == "" {
		permissions, err := session.UserChannelPermissions(session.State.User.ID, voice.ChannelID)
		if err != nil || permissions&(discordgo.PermissionVoiceConnect|discordgo.PermissionVoiceSpeak) != (discordgo.PermissionVoiceConnect|discordgo.PermissionVoiceSpeak) {
			return resources.Get("music.permissions"), false
		}
		channel, err := session.State.Channel(voice.ChannelID)
		if err != nil || channel.Type != discordgo.ChannelTypeGuildVoice {
			return resources.Get("music.voice_only"), false
		}
		if err := m.backend.join(message.GuildID, voice.ChannelID); err != nil {
			if errors.Is(err, errMusicPlayerLimit) {
				return resources.Get("music.player_limit"), false
			}
			log.Printf("[music] join failed: %v", err)
			return resources.Get("music.failed"), false
		}
		g.channel = voice.ChannelID
	}
	g.textChannel = message.ChannelID
	text := resources.Format("music.queued", map[string]any{"title": musicTitle(tracks[0])})
	if g.current == nil {
		if err := m.startLocked(ctx, message.GuildID, g, tracks[0]); err != nil {
			log.Printf("[music] play failed: %v", err)
			_ = m.stopLocked(ctx, message.GuildID, g)
			return resources.Get("music.failed"), false
		}
		text = resources.Format("music.current", map[string]any{"title": musicTitle(tracks[0])})
		tracks = tracks[1:]
	}
	g.queue = append(g.queue, tracks...)
	return text, true
}

func moveMusicTrack(queue []lavalink.Track, from, to int) {
	track := queue[from]
	if from < to {
		copy(queue[from:to], queue[from+1:to+1])
	} else {
		copy(queue[to+1:from+1], queue[to:from])
	}
	queue[to] = track
}

func musicPosition(g *musicGuild, now time.Time) lavalink.Duration {
	position := g.position
	if g.positionSource != nil {
		position = g.positionSource()
	} else if !g.paused && !g.positionAt.IsZero() {
		position += lavalink.Duration(now.Sub(g.positionAt).Milliseconds())
	}
	if g.current != nil && !g.current.Info.IsStream && g.current.Info.Length > 0 {
		position = min(position, g.current.Info.Length)
	}
	return max(0, position)
}

func musicTitle(track lavalink.Track) string {
	title := strings.TrimSpace(track.Info.Title)
	if title == "" {
		title = "Без названия"
	}
	return truncateMessage(strings.ReplaceAll(strings.ReplaceAll(title, "\n", " "), "@", "＠"), 100)
}
func (m *musicService) startLocked(ctx context.Context, id string, g *musicGuild, track lavalink.Track) error {
	// A unique token distinguishes stale end events when the same song is replayed.
	tagged, err := track.WithUserData(map[string]uint64{"nyandePlay": m.sequence.Add(1)})
	if err != nil {
		return err
	}
	if err := m.backend.update(ctx, id, disgolink.WithTrack(tagged), disgolink.WithPaused(false), disgolink.WithVolume(g.volume)); err != nil {
		return err
	}
	if direct, ok := m.backend.(*directMusicBackend); ok {
		g.positionSource = direct.positionSource(id)
	}
	g.current = &tagged
	g.paused = false
	g.position = 0
	g.positionAt = time.Now()
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
	g.repeat = "off"
	g.paused = false
	g.position = 0
	g.current = nil
	g.positionSource = nil
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
	ctx, cancel := context.WithTimeout(m.ctx, musicPlayTimeout)
	defer cancel()
	var err error
	if !failed && g.repeat == "track" {
		err = m.startLocked(ctx, id, g, *g.current)
	} else {
		if !failed && g.repeat == "queue" {
			g.queue = append(g.queue, *g.current)
		}
		err = m.nextLocked(ctx, id, g)
	}
	if err != nil {
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
	if direct, ok := m.backend.(*directMusicBackend); ok {
		direct.voiceState(e)
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
	if direct, ok := m.backend.(*directMusicBackend); ok {
		direct.voiceServer(e)
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
