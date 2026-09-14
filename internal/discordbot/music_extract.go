package discordbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/mmionya/nyande-bot/internal/config"
	"golang.org/x/net/html"
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
			return nil, fmt.Errorf("music executable %s: %w", path, err)
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
		return 0, errors.New("music process output limit exceeded")
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
	cmd := e.command(ctx, e.cfg.YTDownloadPath, append(e.args(), args...)...)
	configureMusicProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	out := &boundedOutput{limit: 4 << 20}
	diagnostic := &boundedOutput{limit: 32 << 10}
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp: %w (check source availability and yt-dlp version)", err)
	}
	return out.Bytes(), nil
}

type extractedMusic struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Track      string  `json:"track"`
	Artist     string  `json:"artist"`
	Uploader   string  `json:"uploader"`
	Duration   float64 `json:"duration"`
	URL        string  `json:"url"`
	WebpageURL string  `json:"webpage_url"`
	Thumbnail  string  `json:"thumbnail"`
	Thumbnails []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
	Live    bool              `json:"is_live"`
	Headers map[string]string `json:"http_headers"`
	Entries []extractedMusic  `json:"entries"`
}

func musicSearchTarget(identifier string) (string, string, error) {
	return musicSearchTargetLimit(identifier, 20)
}
func musicSearchTargetLimit(identifier string, limit int) (string, string, error) {
	if strings.HasPrefix(identifier, "https://") {
		valid, err := musicIdentifier(identifier)
		return valid, "", err
	}
	prefix, query, ok := strings.Cut(identifier, ":")
	if !ok || strings.TrimSpace(query) == "" {
		return "", "", errors.New("invalid search")
	}
	switch prefix {
	case "ytsearch":
		return fmt.Sprintf("ytsearch%d:%s", limit, query), "youtube", nil
	case "ytmsearch":
		return "https://music.youtube.com/search?q=" + url.QueryEscape(query) + "#songs", "youtube music", nil
	case "scsearch":
		return fmt.Sprintf("scsearch%d:%s", limit, query), "soundcloud", nil
	case "bcsearch":
		return "https://bandcamp.com/search?item_type=t&q=" + url.QueryEscape(query), "bandcamp", nil
	default:
		return "", "", errors.New("unsupported search")
	}
}
func (e *musicExtractor) load(ctx context.Context, identifier string) ([]lavalink.Track, error) {
	return e.loadLimit(ctx, identifier, 20)
}
func (e *musicExtractor) loadLimit(ctx context.Context, identifier string, limit int) ([]lavalink.Track, error) {
	limit = max(1, min(limit, 20))
	target, source, err := musicSearchTargetLimit(identifier, limit)
	if err != nil {
		return nil, err
	}
	if err = e.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-e.gate }()
	if strings.HasPrefix(identifier, "bcsearch:") {
		tracks, err := e.bandcamp(ctx, target)
		return tracks[:min(len(tracks), limit)], err
	}
	data, err := e.run(ctx, "--flat-playlist", "--playlist-end", fmt.Sprint(limit), "--dump-single-json", "--", target)
	if err != nil {
		return nil, err
	}
	var result extractedMusic
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	entries := result.Entries
	if entries == nil {
		entries = []extractedMusic{result}
	}
	tracks := make([]lavalink.Track, 0, min(len(entries), limit))
	for _, entry := range entries[:min(len(entries), limit)] {
		if track, ok := extractedTrack(entry, source); ok {
			tracks = append(tracks, track)
		}
	}
	return tracks, nil
}
func extractedTrack(entry extractedMusic, source string) (lavalink.Track, bool) {
	page := entry.WebpageURL
	if page == "" {
		page = entry.URL
	}
	// Flat YouTube results sometimes carry only the video ID.
	if !strings.Contains(page, "://") && strings.HasPrefix(source, "youtube") && entry.ID != "" {
		page = "https://www.youtube.com/watch?v=" + url.QueryEscape(entry.ID)
	}
	if strings.HasPrefix(page, "http://") {
		page = "https://" + strings.TrimPrefix(page, "http://")
	}
	if _, err := musicIdentifier(page); err != nil || !strings.HasPrefix(page, "https://") {
		return lavalink.Track{}, false
	}
	if source == "" {
		u, _ := url.Parse(page)
		switch {
		case strings.Contains(u.Hostname(), "soundcloud.com"):
			source = "soundcloud"
		case strings.Contains(u.Hostname(), "bandcamp.com"):
			source = "bandcamp"
		default:
			source = "youtube"
		}
	}
	title := entry.Title
	if entry.Track != "" {
		title = entry.Track
	}
	author := entry.Artist
	if author == "" {
		author = entry.Uploader
	}
	art := entry.Thumbnail
	if art == "" && len(entry.Thumbnails) > 0 {
		art = entry.Thumbnails[len(entry.Thumbnails)-1].URL
	}
	info := lavalink.TrackInfo{Identifier: entry.ID, Title: truncateMessage(title, 200), Author: truncateMessage(author, 100), URI: &page, SourceName: source, IsStream: entry.Live}
	if entry.Duration > 0 && entry.Duration < 7*24*3600 {
		info.Length = lavalink.Duration(entry.Duration * 1000)
	}
	if musicURL(art) != "" {
		info.ArtworkURL = &art
	}
	encoded, _ := json.Marshal(info)
	return lavalink.Track{Encoded: string(encoded), Info: info}, true
}

type musicStream struct {
	URL     string
	Headers map[string]string
}

func (e *musicExtractor) resolve(ctx context.Context, track lavalink.Track) (musicStream, error) {
	if track.Info.URI == nil {
		return musicStream{}, errors.New("track has no source URL")
	}
	page, err := musicIdentifier(*track.Info.URI)
	if err != nil || !strings.HasPrefix(page, "https://") {
		return musicStream{}, errors.New("invalid track URL")
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
		return musicStream{}, errors.New("extractor did not return an HTTP audio stream")
	}
	return musicStream{URL: info.URL, Headers: info.Headers}, nil
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func htmlClass(n *html.Node, class string) bool {
	for _, v := range strings.Fields(htmlAttr(n, "class")) {
		if v == class {
			return true
		}
	}
	return false
}
func htmlText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(htmlText(c))
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
func walkHTML(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, visit)
	}
}
func (e *musicExtractor) bandcamp(ctx context.Context, target string) ([]lavalink.Track, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; nyande music search)")
	res, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bandcamp search HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("Bandcamp search page too large")
	}
	if bytes.Contains(data, []byte("Client Challenge")) {
		return nil, errors.New("Bandcamp requires a browser challenge")
	}
	return parseBandcampSearch(data)
}
func parseBandcampSearch(data []byte) ([]lavalink.Track, error) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tracks := make([]lavalink.Track, 0, 20)
	walkHTML(doc, func(n *html.Node) {
		if !htmlClass(n, "searchresult") || len(tracks) >= 20 {
			return
		}
		var entry extractedMusic
		walkHTML(n, func(child *html.Node) {
			if htmlClass(child, "heading") {
				entry.Title = htmlText(child)
				walkHTML(child, func(a *html.Node) {
					if a.Data == "a" {
						entry.WebpageURL = htmlAttr(a, "href")
					}
				})
			}
			if htmlClass(child, "subhead") {
				entry.Artist = strings.TrimSpace(strings.TrimPrefix(htmlText(child), "by "))
			}
			if child.Data == "img" && entry.Thumbnail == "" {
				entry.Thumbnail = htmlAttr(child, "src")
			}
		})
		u, err := url.Parse(entry.WebpageURL)
		if err != nil || !strings.HasPrefix(u.Path, "/track/") {
			return
		}
		entry.ID = strings.TrimPrefix(u.Path, "/track/")
		if track, ok := extractedTrack(entry, "bandcamp"); ok {
			tracks = append(tracks, track)
		}
	})
	return tracks, nil
}
