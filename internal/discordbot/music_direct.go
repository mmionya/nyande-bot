package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/disgolink/v4/disgolink"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/disgoorg/godave"
	"github.com/disgoorg/snowflake/v2"
	"github.com/mmionya/nyande-bot/internal/config"
	davesession "github.com/thomas-vilte/dave-go/session"
)

// The existing queue/UI DTOs are shared with the optional legacy transport.
// This transport opens no Lavalink connection and starts no Java process.
type musicVoice interface {
	open(context.Context, snowflake.ID) error
	close(context.Context)
	write([]byte) error
	speaking(context.Context, bool) error
	holdFrames() bool
	voiceState(gateway.EventVoiceStateUpdate)
	voiceServer(gateway.EventVoiceServerUpdate)
}
type directVoice struct {
	conn       voice.Conn
	dave       *davesession.Session
	eventMu    sync.Mutex
	closed     bool
	stateReady bool
	server     *gateway.EventVoiceServerUpdate
}

// DisGo v0.19.6 passes a zero-length output slice with reserved capacity to
// godave.Encrypt. The pure-Go implementation writes into len(dst), so expose
// the allocated region; otherwise even transport-only audio becomes empty.
type musicDaveSession struct{ godave.Session }

func (s *musicDaveSession) Encrypt(ssrc uint32, frame, dst []byte) (int, error) {
	return s.Session.Encrypt(ssrc, frame, dst[:cap(dst)])
}

func (v *directVoice) open(ctx context.Context, ch snowflake.ID) error {
	return v.conn.Open(ctx, ch, false, true)
}
func (v *directVoice) close(ctx context.Context) {
	v.eventMu.Lock()
	defer v.eventMu.Unlock()
	if v.closed {
		return
	}
	v.closed = true
	v.conn.Close(ctx)
	_ = v.dave.Close()
}
func (v *directVoice) write(frame []byte) error {
	if err := v.conn.UDP().SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	_, err := v.conn.UDP().Write(frame)
	return err
}
func (v *directVoice) speaking(ctx context.Context, on bool) error {
	flag := voice.SpeakingFlagNone
	if on {
		flag = voice.SpeakingFlagMicrophone
	}
	return v.conn.SetSpeaking(ctx, flag)
}
func (v *directVoice) holdFrames() bool { return v.dave.ShouldHoldFrames() }
func (v *directVoice) voiceState(e gateway.EventVoiceStateUpdate) {
	v.eventMu.Lock()
	defer v.eventMu.Unlock()
	if v.closed {
		return
	}
	v.conn.HandleVoiceStateUpdate(e)
	v.stateReady = e.ChannelID != nil && e.SessionID != ""
	if v.stateReady && v.server != nil {
		v.conn.HandleVoiceServerUpdate(*v.server)
		v.server = nil
	}
}
func (v *directVoice) voiceServer(e gateway.EventVoiceServerUpdate) {
	v.eventMu.Lock()
	defer v.eventMu.Unlock()
	if v.closed {
		return
	}
	if !v.stateReady {
		v.server = &e
		return
	}
	v.conn.HandleVoiceServerUpdate(e)
}

type directMusicBackend struct {
	service    *musicService
	extractor  *musicExtractor
	maxPlayers int
	mu         sync.Mutex
	players    map[string]*directMusicPlayer
	newVoice   func(string, func()) (musicVoice, error)
	encoder    func(context.Context, string, musicStream, int64, int) (*musicEncoder, error)
}
type directMusicPlayer struct {
	mu      sync.Mutex // control operations; gateway events must not take this lock
	conn    musicVoice
	channel string
	closed  atomic.Bool
	active  atomic.Pointer[directMusicRun]
	volume  int
}
type directMusicRun struct {
	track    lavalink.Track
	stream   musicStream
	encoder  *musicEncoder
	cancel   context.CancelFunc
	done     chan struct{}
	paused   atomic.Bool
	position atomic.Int64
}

func (r *directMusicRun) stop() { r.cancel(); <-r.done }

func newDirectMusicBackend(m *musicService, s *discordgo.Session, cfg config.Config) (*directMusicBackend, error) {
	extractor, err := newMusicExtractor(cfg)
	if err != nil {
		return nil, err
	}
	user, err := snowflake.Parse(s.State.User.ID)
	if err != nil {
		return nil, err
	}
	d := &directMusicBackend{service: m, extractor: extractor, maxPlayers: max(1, cfg.MusicMaxPlayers), players: map[string]*directMusicPlayer{}, encoder: startMusicEncoder}
	d.newVoice = func(id string, onClose func()) (musicVoice, error) {
		guild, err := snowflake.Parse(id)
		if err != nil {
			return nil, err
		}
		v := &directVoice{}
		var once sync.Once
		v.conn = voice.NewConn(guild, user, func(ctx context.Context, guild snowflake.ID, ch *snowflake.ID, mute, deaf bool) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			channel := ""
			if ch != nil {
				channel = ch.String()
			}
			return s.ChannelVoiceJoinManual(guild.String(), channel, mute, deaf)
		}, func() { once.Do(onClose) },
			voice.WithConnGatewayConfigOpts(voice.WithGatewayAutoReconnect(false)),
			voice.WithConnDaveSessionCreateFunc(func(logger *slog.Logger, user godave.UserID, callbacks godave.Callbacks) godave.Session {
				v.dave = davesession.New(user, callbacks, davesession.WithLogger(logger))
				return &musicDaveSession{Session: v.dave}
			}))
		return v, nil
	}
	return d, nil
}
func (d *directMusicBackend) player(id string) *directMusicPlayer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.players[id]
}
func (d *directMusicBackend) load(ctx context.Context, query string) ([]lavalink.Track, error) {
	return d.extractor.load(ctx, query)
}

var errMusicPlayerLimit = errors.New("music player limit reached")

func (d *directMusicBackend) join(id, channel string) error {
	if channel == "" {
		return nil
	} // destroy already leaves the voice channel
	ch, err := snowflake.Parse(channel)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if p := d.players[id]; p != nil {
		d.mu.Unlock()
		if p.channel != channel || p.closed.Load() {
			return errors.New("voice player already exists")
		}
		return nil
	}
	if len(d.players) >= d.maxPlayers {
		d.mu.Unlock()
		return errMusicPlayerLimit
	}
	p := &directMusicPlayer{channel: channel, volume: 50}
	conn, err := d.newVoice(id, func() {
		if !p.closed.Load() {
			go d.service.voiceClosed(id)
		}
	})
	if err != nil {
		d.mu.Unlock()
		return err
	}
	p.conn = conn
	d.players[id] = p
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.service.ctx, 15*time.Second)
	defer cancel()
	p.mu.Lock()
	err = conn.open(ctx, ch)
	p.mu.Unlock()
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.destroy(cleanup, id)
	}
	return err
}
func (d *directMusicBackend) destroy(ctx context.Context, id string) error {
	p := d.player(id)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Swap(true) {
		return nil
	}
	if run := p.active.Swap(nil); run != nil {
		run.stop()
	}
	p.conn.close(ctx)
	d.mu.Lock()
	if d.players[id] == p {
		delete(d.players, id)
	}
	d.mu.Unlock()
	return nil
}
func (d *directMusicBackend) close() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.players))
	for id := range d.players {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.destroy(ctx, id)
		cancel()
	}
}
func (d *directMusicBackend) positionSource(id string) func() lavalink.Duration {
	p := d.player(id)
	return func() lavalink.Duration {
		if p != nil {
			if r := p.active.Load(); r != nil {
				return lavalink.Duration(r.position.Load())
			}
		}
		return 0
	}
}
func (d *directMusicBackend) update(ctx context.Context, id string, opts ...disgolink.PlayerUpdateOpt) error {
	var update lavalink.PlayerUpdate
	for _, opt := range opts {
		opt(&update)
	}
	p := d.player(id)
	if p == nil {
		return errors.New("voice is not connected")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return errors.New("voice player is closed")
	}
	volume := p.volume
	if update.Volume != nil {
		volume = *update.Volume
	}
	if volume < 0 || volume > 100 {
		return errors.New("invalid volume")
	}
	old := p.active.Load()
	if update.Track != nil {
		encoded := update.Track.Encoded.Value
		if encoded == nil {
			return errors.New("missing direct track metadata")
		}
		track := lavalink.Track{Encoded: *encoded}
		if err := json.Unmarshal([]byte(*encoded), &track.Info); err != nil {
			return err
		}
		data, err := json.Marshal(update.Track.UserData)
		if err != nil {
			return err
		}
		track.UserData = data
		stream, err := d.extractor.resolve(ctx, track)
		if err != nil {
			return err
		}
		return d.replaceRun(p, id, track, stream, 0, volume, false)
	}
	if old == nil {
		return errors.New("nothing is playing")
	}
	paused := old.paused.Load()
	if update.Paused != nil {
		paused = *update.Paused
	}
	if update.Volume != nil && volume != p.volume {
		return d.replaceRun(p, id, old.track, old.stream, old.position.Load(), volume, paused)
	}
	old.paused.Store(paused)
	return nil
}
func (d *directMusicBackend) replaceRun(p *directMusicPlayer, id string, track lavalink.Track, stream musicStream, offset int64, volume int, paused bool) error {
	if track.Info.IsStream {
		offset = 0
	}
	// Spawn failure leaves the old run intact. Once started, the previous process
	// is cancelled and reaped before the new sender starts writing to UDP.
	ctx, cancel := context.WithCancel(d.service.ctx)
	encoder, err := d.encoder(ctx, d.extractor.cfg.FFmpegPath, stream, offset, volume)
	if err != nil {
		cancel()
		return err
	}
	r := &directMusicRun{track: track, stream: stream, encoder: encoder, cancel: cancel, done: make(chan struct{})}
	r.position.Store(offset)
	r.paused.Store(paused)
	if old := p.active.Load(); old != nil {
		old.stop()
	}
	p.active.Store(r)
	p.volume = volume
	go d.send(ctx, p, id, r)
	return nil
}
func (d *directMusicBackend) send(ctx context.Context, p *directMusicPlayer, id string, r *directMusicRun) {
	defer close(r.done)
	defer r.encoder.stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	speaking := false
	heldSince := time.Time{}
	emptySince := time.Now()
	finish := func(err error) {
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[music] direct playback failed guild=%s: %v", id, err)
		}
		go d.service.trackEnded(id, r.track, err != nil)
	}
	setSpeaking := func(on bool) error {
		if speaking == on {
			return nil
		}
		callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		err := p.conn.speaking(callCtx, on)
		if err == nil {
			speaking = on
		}
		return err
	}
	defer func() {
		callCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.conn.speaking(callCtx, false)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.paused.Load() {
				if err := setSpeaking(false); err != nil {
					finish(err)
					return
				}
				emptySince = time.Now()
				continue
			}
			if p.conn.holdFrames() {
				if heldSince.IsZero() {
					heldSince = time.Now()
				}
				if time.Since(heldSince) > 30*time.Second {
					finish(errors.New("DAVE handshake timed out"))
					return
				}
				emptySince = time.Now()
				continue
			}
			heldSince = time.Time{}
			select {
			case frame, ok := <-r.encoder.frames:
				if !ok {
					<-r.encoder.done
					finish(r.encoder.err)
					return
				}
				if err := setSpeaking(true); err != nil {
					finish(err)
					return
				}
				if err := p.conn.write(frame); err != nil {
					finish(fmt.Errorf("voice send: %w", err))
					return
				}
				r.position.Add(20)
				emptySince = time.Now()
			default:
				if time.Since(emptySince) > 30*time.Second {
					finish(errors.New("audio stream stalled"))
					return
				}
			}
		}
	}
}
func (d *directMusicBackend) voiceState(e *discordgo.VoiceStateUpdate) {
	p := d.player(e.GuildID)
	if p == nil || p.closed.Load() {
		return
	}
	guild, err := snowflake.Parse(e.GuildID)
	if err != nil {
		return
	}
	user, err := snowflake.Parse(e.UserID)
	if err != nil {
		return
	}
	var channel *snowflake.ID
	if e.ChannelID != "" {
		id, err := snowflake.Parse(e.ChannelID)
		if err != nil {
			return
		}
		channel = &id
	}
	p.conn.voiceState(gateway.EventVoiceStateUpdate{VoiceState: discord.VoiceState{GuildID: guild, UserID: user, ChannelID: channel, SessionID: e.SessionID, SelfDeaf: e.SelfDeaf, SelfMute: e.SelfMute}})
	// An external disconnect/move invalidates the current stream. Stop cleanly;
	// the next /play can establish a fresh DAVE session in the user's channel.
	if e.ChannelID != p.channel {
		go d.service.voiceClosed(e.GuildID)
	}
}
func (d *directMusicBackend) voiceServer(e *discordgo.VoiceServerUpdate) {
	p := d.player(e.GuildID)
	if p == nil || p.closed.Load() {
		return
	}
	guild, err := snowflake.Parse(e.GuildID)
	if err != nil {
		return
	}
	if e.Endpoint == "" {
		go d.service.voiceClosed(e.GuildID)
		return
	}
	p.conn.voiceServer(gateway.EventVoiceServerUpdate{GuildID: guild, Token: e.Token, Endpoint: &e.Endpoint})
}
