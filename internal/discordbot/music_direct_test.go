package discordbot

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/disgolink/v4/disgolink"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/disgoorg/snowflake/v2"
	"github.com/mmionya/nyande-bot/internal/config"
	davesession "github.com/thomas-vilte/dave-go/session"
)

func TestDirectSearchSourcesAndMetadata(t *testing.T) {
	cases := map[string]string{"ytsearch:a b": "ytsearch20:a b", "ytmsearch:a b": "https://music.youtube.com/search?q=a+b#songs", "scsearch:hello": "scsearch20:hello", "bcsearch:a&b": "https://bandcamp.com/search?item_type=t&q=a%26b"}
	for query, want := range cases {
		got, _, err := musicSearchTarget(query)
		if err != nil || got != want {
			t.Fatalf("%s: %q, %v", query, got, err)
		}
	}
	for _, query := range []string{"file:///etc/passwd", "https://localhost/audio", "bcsearch:", "arbitrary:query"} {
		if _, _, err := musicSearchTarget(query); err == nil {
			t.Fatalf("accepted %q", query)
		}
	}
	track, ok := extractedTrack(extractedMusic{ID: "abc", URL: "abc", Title: "song", Uploader: "artist", Duration: 123.5, Thumbnail: "https://i.ytimg.com/test.jpg"}, "youtube")
	if !ok || *track.Info.URI != "https://www.youtube.com/watch?v=abc" || track.Info.Length != 123500 || track.Info.Author != "artist" {
		t.Fatalf("bad metadata: %+v", track)
	}
	var info lavalink.TrackInfo
	if err := json.Unmarshal([]byte(track.Encoded), &info); err != nil || info.Title != "song" {
		t.Fatal("invalid direct metadata", err)
	}
	if _, ok := extractedTrack(extractedMusic{URL: "https://localhost/audio"}, "soundcloud"); ok {
		t.Fatal("accepted unsupported result")
	}
	tracks, err := parseBandcampSearch([]byte(`<ul><li class="searchresult data-search"><div class="heading"><a href="https://artist.bandcamp.com/track/song?from=search">A &amp; B</a></div><div class="subhead">by Artist</div><img src="https://f4.bcbits.com/img/a.jpg"></li><li class="searchresult"><div class="heading"><a href="https://artist.bandcamp.com/album/no">Album</a></div></li></ul>`))
	if err != nil || len(tracks) != 1 || tracks[0].Info.Title != "A & B" || tracks[0].Info.Author != "Artist" {
		t.Fatalf("Bandcamp parse: %+v, %v", tracks, err)
	}
}

func TestDirectExtractorProcessesBoundedAndCancelled(t *testing.T) {
	// Test the process boundary without public sites, tokens or downloading audio.
	script := filepath.Join(t.TempDir(), "extractor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$*\" in\n*hang*) exec sleep 30;;\nesac\nprintf '%s' '{\"entries\":[{\"id\":\"abc\",\"url\":\"https://www.youtube.com/watch?v=abc\",\"title\":\"song\",\"duration\":12}]}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &musicExtractor{cfg: config.Config{YTDownloadPath: script}, gate: make(chan struct{}, 1), command: exec.CommandContext}
	tracks, err := e.load(context.Background(), "ytsearch:ok")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("load: %+v %v", tracks, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := e.load(ctx, "ytsearch:hang"); err == nil {
		t.Fatal("extractor ignored cancellation")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("extractor did not exit promptly")
	}
	e.gate <- struct{}{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if err := e.acquire(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("gate ignored request deadline", err)
	}
	<-e.gate
	out := &boundedOutput{limit: 4}
	if _, err := out.Write([]byte("12345")); err == nil || len(out.Bytes()) != 0 {
		t.Fatal("output cap not enforced")
	}
}

func oggPage(sequence uint32, flags byte, lacing []byte, body []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint32(h[14:18], 1)
	binary.LittleEndian.PutUint32(h[18:22], sequence)
	h[26] = byte(len(lacing))
	return append(append(h, lacing...), body...)
}
func TestOggOpusContinuedPacketsAndTruncation(t *testing.T) {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[9] = 2
	prefix := oggPage(0, 2, []byte{19, 8}, append(head, []byte("OpusTags")...))
	packet := bytes.Repeat([]byte{0xfc}, 300)
	data := append(bytes.Clone(prefix), oggPage(1, 0, []byte{255}, packet[:255])...)
	data = append(data, oggPage(2, 1, []byte{45}, packet[255:])...)
	r := oggOpusReader{r: bytes.NewReader(data)}
	got, err := r.next()
	if err != nil || !bytes.Equal(got, packet) {
		t.Fatalf("continued packet: %d, %v", len(got), err)
	}
	if _, err = r.next(); err != io.EOF {
		t.Fatal("expected EOF", err)
	}
	r = oggOpusReader{r: bytes.NewReader(data[:len(data)-1])}
	if _, err = r.next(); err == nil {
		t.Fatal("accepted truncated page")
	}
	r = oggOpusReader{r: bytes.NewReader(append(prefix, oggPage(1, 0, []byte{255}, packet[:255])...))}
	if _, err = r.next(); err != io.ErrUnexpectedEOF {
		t.Fatal("accepted unfinished packet", err)
	}
}
func TestMusicFFmpegArguments(t *testing.T) {
	args := musicFFmpegArgs(musicStream{URL: "https://cdn.example/audio?x=1", Headers: map[string]string{"User-Agent": "agent", "Referer": "bad\r\nInjected: yes", "Cookie": "secret"}}, 12340, 30)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-ss 12.340", "volume=0.30", "-frame_duration 20", "-b:a 64k", "-threads 1", "User-Agent: agent"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing argument", want)
		}
	}
	if strings.Contains(joined, "Injected") || strings.Contains(joined, "secret") {
		t.Fatal("unsafe HTTP headers passed")
	}
}
func TestDirectFFmpegHTTPStreamAndCancellation(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	// 1 second of stereo PCM with a WAV header, served with HTTP Range support.
	data := make([]byte, 44+48000*4)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 2)
	binary.LittleEndian.PutUint32(data[24:28], 48000)
	binary.LittleEndian.PutUint32(data[28:32], 192000)
	binary.LittleEndian.PutUint16(data[32:34], 4)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(len(data)-44))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "test.wav", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	encoder, err := startMusicEncoder(ctx, path, musicStream{URL: server.URL}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	frames := 0
	for frame := range encoder.frames {
		if len(frame) == 0 {
			t.Fatal("empty frame")
		}
		frames++
	}
	<-encoder.done
	if encoder.err != nil || frames < 50 || frames > 52 {
		t.Fatalf("frames=%d, error=%v", frames, encoder.err)
	}
	encoder, err = startMusicEncoder(ctx, path, musicStream{URL: server.URL}, 500, 30)
	if err != nil {
		t.Fatal(err)
	}
	frames = 0
	for range encoder.frames {
		frames++
	}
	<-encoder.done
	if encoder.err != nil || frames < 25 || frames > 27 {
		t.Fatalf("seek frames=%d, error=%v", frames, encoder.err)
	}
	encoder, err = startMusicEncoderCommand(context.Background(), path, []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-c:a", "libopus", "-frame_duration", "20", "-f", "opus", "-page_duration", "20000", "pipe:1"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-encoder.frames:
	case <-time.After(3 * time.Second):
		encoder.stop()
		t.Fatal("encoder produced no frames")
	}
	start := time.Now()
	encoder.stop()
	if time.Since(start) > 3*time.Second {
		t.Fatal("stopped encoder was not reaped")
	}
}

type fakeDirectVoice struct {
	writes   atomic.Int64
	hold     atomic.Bool
	closed   atomic.Bool
	opens    atomic.Int64
	states   atomic.Int64
	servers  atomic.Int64
	openHook func() error
	writeErr error
}

func (v *fakeDirectVoice) open(context.Context, snowflake.ID) error {
	v.opens.Add(1)
	if v.openHook != nil {
		return v.openHook()
	}
	return nil
}
func (v *fakeDirectVoice) close(context.Context)                      { v.closed.Store(true) }
func (v *fakeDirectVoice) write([]byte) error                         { v.writes.Add(1); return v.writeErr }
func (v *fakeDirectVoice) speaking(context.Context, bool) error       { return nil }
func (v *fakeDirectVoice) holdFrames() bool                           { return v.hold.Load() }
func (v *fakeDirectVoice) voiceState(gateway.EventVoiceStateUpdate)   { v.states.Add(1) }
func (v *fakeDirectVoice) voiceServer(gateway.EventVoiceServerUpdate) { v.servers.Add(1) }
func fakeDirectEncoder(ctx context.Context, _ string, _ musicStream, _ int64, _ int) (*musicEncoder, error) {
	ctx, cancel := context.WithCancel(ctx)
	p := &musicEncoder{frames: make(chan []byte, 2), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(p.done)
		defer close(p.frames)
		for {
			select {
			case p.frames <- []byte{0xf8, 0xff, 0xfe}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return p, nil
}
func testDirectBackend(t *testing.T) (*directMusicBackend, *fakeDirectVoice, *musicService) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &musicService{ctx: ctx, notify: func(string, string) {}}
	v := &fakeDirectVoice{}
	d := &directMusicBackend{service: m, extractor: &musicExtractor{cfg: config.Config{FFmpegPath: "ffmpeg"}}, players: map[string]*directMusicPlayer{}, maxPlayers: 1, encoder: fakeDirectEncoder, newVoice: func(string, func()) (musicVoice, error) { return v, nil }}
	m.backend = d
	t.Cleanup(d.close)
	return d, v, m
}
func waitMusic(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for music state")
}
func TestDirectPlaybackPauseVolumeDAVEAndStop(t *testing.T) {
	d, v, _ := testDirectBackend(t)
	if err := d.join("1", "10"); err != nil {
		t.Fatal(err)
	}
	if err := d.join("2", "20"); !errors.Is(err, errMusicPlayerLimit) {
		t.Fatal("player cap ignored", err)
	}
	p := d.player("1")
	v.hold.Store(true)
	if err := d.replaceRun(p, "1", musicTrack("test"), musicStream{}, 0, 50, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	if v.writes.Load() != 0 {
		t.Fatal("sent audio before DAVE ready")
	}
	v.hold.Store(false)
	waitMusic(t, func() bool { return v.writes.Load() >= 3 })
	if err := d.update(context.Background(), "1", disgolink.WithPaused(true)); err != nil {
		t.Fatal(err)
	}
	// A frame already in flight may finish; thereafter the position stays frozen.
	time.Sleep(30 * time.Millisecond)
	position := p.active.Load().position.Load()
	time.Sleep(70 * time.Millisecond)
	if p.active.Load().position.Load() != position {
		t.Fatal("paused track advanced")
	}
	old := p.active.Load()
	if err := d.update(context.Background(), "1", disgolink.WithVolume(20)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.done:
	default:
		t.Fatal("volume change leaked old sender")
	}
	if !p.active.Load().paused.Load() || p.active.Load().position.Load() != position || p.volume != 20 {
		t.Fatal("volume lost pause/position")
	}
	if err := d.update(context.Background(), "1", disgolink.WithPaused(false)); err != nil {
		t.Fatal(err)
	}
	waitMusic(t, func() bool { return p.active.Load().position.Load() > position })
	current := p.active.Load()
	if err := d.destroy(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-current.done:
	default:
		t.Fatal("sender survived stop")
	}
	if !v.closed.Load() || d.player("1") != nil {
		t.Fatal("voice survived stop")
	}
	if err := d.join("2", "20"); err != nil {
		t.Fatal("player slot not released", err)
	}
}
func TestDirectConcurrentGuildAdmission(t *testing.T) {
	d, _, _ := testDirectBackend(t)
	var successes atomic.Int64
	var wg sync.WaitGroup
	for _, id := range []string{"1", "2", "3", "4"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.join(id, "10"); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("admitted %d players", successes.Load())
	}
}

func TestDirectTrackEndAdvancesQueue(t *testing.T) {
	d, _, m := testDirectBackend(t)
	// Use an actual child process for source resolution, but no external network.
	d.extractor.gate = make(chan struct{}, 1)
	d.extractor.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf '%s' '{"url":"https://cdn.example/audio"}'`)
	}
	var created atomic.Int64
	d.encoder = func(ctx context.Context, path string, stream musicStream, offset int64, volume int) (*musicEncoder, error) {
		if created.Add(1) > 1 {
			return fakeDirectEncoder(ctx, path, stream, offset, volume)
		}
		p := &musicEncoder{frames: make(chan []byte, 1), done: make(chan struct{}), cancel: func() {}}
		p.frames <- []byte{0xf8, 0xff, 0xfe}
		close(p.frames)
		close(p.done)
		return p, nil
	}
	first, _ := extractedTrack(extractedMusic{URL: "https://www.youtube.com/watch?v=first", Title: "first"}, "youtube")
	second, _ := extractedTrack(extractedMusic{URL: "https://www.youtube.com/watch?v=second", Title: "second"}, "youtube")
	if err := d.join("1", "10"); err != nil {
		t.Fatal(err)
	}
	g := m.guild("1")
	g.mu.Lock()
	g.channel = "10"
	g.queue = []lavalink.Track{second}
	err := m.startLocked(context.Background(), "1", g, first)
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitMusic(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.current != nil && g.current.Info.Title == "second"
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.queue) != 0 || musicPlayID(*g.current) == 0 || created.Load() != 2 {
		t.Fatal("end event did not advance once with a valid play ID")
	}
}

func TestDirectVoiceEventsUnblockJoin(t *testing.T) {
	d, v, m := testDirectBackend(t)
	b, _ := testMusicBot(t)
	b.music = m
	ready := make(chan struct{})
	v.openHook = func() error {
		go func() {
			b.handleMusicVoiceServer(b.session, &discordgo.VoiceServerUpdate{GuildID: "1", Endpoint: "voice.example", Token: "test"})
			b.handleMusicVoiceState(b.session, &discordgo.VoiceStateUpdate{VoiceState: &discordgo.VoiceState{GuildID: "1", UserID: "99", ChannelID: "10", SessionID: "session"}})
			close(ready)
		}()
		select {
		case <-ready:
			return nil
		case <-time.After(time.Second):
			return errors.New("voice events blocked behind the queue mutex")
		}
	}
	g := m.guild("1")
	g.mu.Lock()
	err := d.join("1", "10")
	g.mu.Unlock()
	if err != nil || v.states.Load() != 1 || v.servers.Load() != 1 {
		t.Fatal("voice events failed", err)
	}
}

// Opt-in smoke test: public source requests, no Discord token or voice sends.
func TestDirectSourceSmoke(t *testing.T) {
	if os.Getenv("NYANDE_MUSIC_SMOKE") != "1" {
		t.Skip("set NYANDE_MUSIC_SMOKE=1 to check public music sources")
	}
	e, err := newMusicExtractor(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"ytsearch:", "ytmsearch:", "scsearch:", "bcsearch:"} {
		t.Run(source, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			tracks, err := e.load(ctx, source+"Kevin MacLeod")
			if err != nil {
				t.Fatal(err)
			}
			if len(tracks) == 0 {
				t.Fatal("no tracks returned")
			}
			t.Logf("%d tracks", len(tracks))
		})
	}
}

func TestMusicDaveReservedOutputBuffer(t *testing.T) {
	session := davesession.New("99", nil)
	defer session.Close()
	adapter := &musicDaveSession{Session: session}
	frame := []byte{0xf8, 0xff, 0xfe}
	dst := make([]byte, 0, session.MaxEncryptedFrameSize(len(frame)))
	n, err := adapter.Encrypt(1, frame, dst)
	if err != nil || n != len(frame) || !bytes.Equal(dst[:n], frame) {
		t.Fatalf("reserved buffer lost audio: bytes=%d err=%v", n, err)
	}
}

func TestDirectStreamSmoke(t *testing.T) {
	if os.Getenv("NYANDE_MUSIC_SMOKE") != "1" {
		t.Skip("set NYANDE_MUSIC_SMOKE=1 to check real stream extraction")
	}
	e, err := newMusicExtractor(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	tracks, err := e.loadLimit(ctx, "scsearch:Kevin MacLeod", 1)
	t.Logf("search: %s", time.Since(started))
	if err != nil || len(tracks) == 0 {
		t.Fatal("search failed", err)
	}
	started = time.Now()
	stream, err := e.resolve(ctx, tracks[0])
	t.Logf("resolve: %s", time.Since(started))
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := startMusicEncoder(ctx, e.cfg.FFmpegPath, stream, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.stop()
	for i := 0; i < 100; i++ {
		select {
		case frame, ok := <-encoder.frames:
			if !ok {
				<-encoder.done
				t.Fatalf("stream ended at frame %d: %v", i, encoder.err)
			}
			if len(frame) == 0 {
				t.Fatal("empty Opus frame")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	t.Log("resolved a real SoundCloud URL and encoded 100 Opus frames")
}

func TestDirectVoiceUDPFrames(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	received := make(chan []byte, 2)
	go func() {
		_ = server.SetDeadline(time.Now().Add(3 * time.Second))
		request := make([]byte, 74)
		n, peer, err := server.ReadFromUDP(request)
		if err != nil || n != 74 {
			return
		}
		response := make([]byte, 74)
		binary.BigEndian.PutUint16(response[:2], 2)
		binary.BigEndian.PutUint16(response[2:4], 70)
		copy(response[4:8], request[4:8])
		copy(response[8:72], "127.0.0.1")
		binary.BigEndian.PutUint16(response[72:74], uint16(peer.Port))
		if _, err = server.WriteToUDP(response, peer); err != nil {
			return
		}
		for i := 0; i < 2; i++ {
			data := make([]byte, 1500)
			n, _, err = server.ReadFromUDP(data)
			if err != nil {
				return
			}
			received <- data[:n]
		}
	}()
	session := davesession.New("99", nil)
	defer session.Close()
	adapter := &musicDaveSession{Session: session}
	conn := voice.NewUDPConn(adapter, func(uint32) snowflake.ID { return 99 })
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err = conn.Open(ctx, "127.0.0.1", server.LocalAddr().(*net.UDPAddr).Port, 7); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{3}, 32)
	if err = conn.SetSecretKey(voice.EncryptionModeAEADAES256GCMRTPSize, key); err != nil {
		t.Fatal(err)
	}
	decrypt, err := voice.NewEncrypter(voice.EncryptionModeAEADAES256GCMRTPSize, key)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0xf8, 0xff, 0xfe}
	for i := 0; i < 2; i++ {
		if _, err = conn.Write(frame); err != nil {
			t.Fatal(err)
		}
		select {
		case packet := <-received:
			if binary.BigEndian.Uint16(packet[2:4]) != uint16(i) || binary.BigEndian.Uint32(packet[4:8]) != uint32(i*960) {
				t.Fatal("invalid RTP timing")
			}
			plain, err := decrypt.Decrypt(12, packet)
			if err != nil || !bytes.Equal(plain, frame) {
				t.Fatalf("UDP audio corrupted: %x %v", plain, err)
			}
		case <-ctx.Done():
			t.Fatal("no UDP voice frame", ctx.Err())
		}
	}
}
