package discordbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/music"
	"github.com/mmionya/nyande-bot/resources"
)

// The gate is shared by searches and playback URL resolution: only one Python
// extractor runs at a time, including requests from different Discord guilds.
type musicExtractor struct {
	cfg     config.Config
	gate    chan struct{}
	client  *http.Client
	command func(context.Context, string, ...string) *exec.Cmd
}

func newMusicExtractor(cfg config.Config) (*musicExtractor, error) {
	if cfg.YTDownloadPath == "" {
		cfg.YTDownloadPath = "yt-dlp"
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	for _, path := range []string{cfg.YTDownloadPath, cfg.FFmpegPath} {
		if _, err := exec.LookPath(path); err != nil {
			return nil, fmt.Errorf(resources.Get("music.error.executable"), path, err)
		}
	}
	return &musicExtractor{cfg: cfg, gate: make(chan struct{}, 1), client: &http.Client{Timeout: 20 * time.Second}, command: exec.CommandContext}, nil
}

// boundedOutput refuses unbounded metadata and never logs stream URLs/cookies.
type boundedOutput struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > b.limit-len(b.data) {
		return 0, errors.New(resources.Get("music.error.output_limit"))
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *boundedOutput) Bytes() []byte { b.mu.Lock(); defer b.mu.Unlock(); return bytes.Clone(b.data) }
func (e *musicExtractor) acquire(ctx context.Context) error {
	select {
	case e.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *musicExtractor) args() []string {
	args := []string{"--ignore-config", "--no-warnings", "--no-progress", "--no-playlist", "--socket-timeout", "10", "--retries", "1", "--extractor-retries", "1", "--no-cache-dir"}
	if e.cfg.YTDownloadCookies != "" {
		args = append(args, "--cookies", e.cfg.YTDownloadCookies)
	}
	if e.cfg.YTDownloadBrowser != "" {
		args = append(args, "--cookies-from-browser", e.cfg.YTDownloadBrowser)
	}
	return args
}
func (e *musicExtractor) run(ctx context.Context, args ...string) ([]byte, error) {
	release, err := music.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	cmd := e.command(ctx, e.cfg.YTDownloadPath, append(e.args(), args...)...)
	configureMusicProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	out := &boundedOutput{limit: 4 << 20}
	diagnostic := &boundedOutput{limit: 32 << 10}
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf(resources.Get("music.error.ytdlp"), err)
	}
	return out.Bytes(), nil
}

type extractedMusic = music.Metadata

func musicSearchTarget(identifier string) (string, string, error) {
	return music.SearchTarget(identifier, 20)
}
func musicSearchTargetLimit(identifier string, limit int) (string, string, error) {
	return music.SearchTarget(identifier, limit)
}
func (e *musicExtractor) load(ctx context.Context, identifier string) ([]lavalink.Track, error) {
	return e.loadLimit(ctx, identifier, 20)
}
func (e *musicExtractor) loadLimit(ctx context.Context, identifier string, limit int) ([]lavalink.Track, error) {
	if err := e.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-e.gate }()
	tracks, err := music.Search(ctx, identifier, limit, e.run, e.client)
	return lavalinkTracks(tracks), err
}
func lavalinkTrack(track music.Track) lavalink.Track {
	info := lavalink.TrackInfo{Identifier: track.ID, Title: track.Title, Author: track.Author, URI: &track.URL, SourceName: track.Source, IsStream: track.Live, Length: lavalink.Duration(track.Duration.Milliseconds())}
	if track.ArtworkURL != "" {
		info.ArtworkURL = &track.ArtworkURL
	}
	encoded, _ := json.Marshal(info)
	return lavalink.Track{Encoded: string(encoded), Info: info}
}
func lavalinkTracks(tracks []music.Track) []lavalink.Track {
	result := make([]lavalink.Track, 0, len(tracks))
	for _, track := range tracks {
		result = append(result, lavalinkTrack(track))
	}
	return result
}
func extractedTrack(entry extractedMusic, source string) (lavalink.Track, bool) {
	track, ok := music.FromMetadata(entry, source)
	return lavalinkTrack(track), ok
}
func parseBandcampSearch(data []byte) ([]lavalink.Track, error) {
	tracks, err := music.ParseBandcampSearch(data)
	return lavalinkTracks(tracks), err
}

type musicStream struct {
	URL     string
	Headers map[string]string
}

func (e *musicExtractor) resolve(ctx context.Context, track lavalink.Track) (musicStream, error) {
	if track.Info.URI == nil {
		return musicStream{}, errors.New(resources.Get("music.error.missing_source_url"))
	}
	page, err := musicIdentifier(*track.Info.URI)
	if err != nil || !strings.HasPrefix(page, "https://") {
		return musicStream{}, errors.New(resources.Get("music.error.invalid_track_url"))
	}
	if err = e.acquire(ctx); err != nil {
		return musicStream{}, err
	}
	defer func() { <-e.gate }()
	data, err := e.run(ctx, "--dump-single-json", "--format", "bestaudio/best", "--", page)
	if err != nil {
		return musicStream{}, err
	}
	var info extractedMusic
	if err = json.Unmarshal(data, &info); err != nil {
		return musicStream{}, err
	}
	u, err := url.Parse(info.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return musicStream{}, errors.New(resources.Get("music.error.missing_audio_stream"))
	}
	return musicStream{URL: info.URL, Headers: info.Headers}, nil
}
