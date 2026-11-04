// Package music shares track discovery between Telegram and Discord.
package music

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
	"golang.org/x/net/html"
)

type Track struct {
	ID, Title, Author, URL, ArtworkURL, Source string
	Duration                                   time.Duration
	Live                                       bool
}

// Keep Python music extraction serialized across both chat transports.
var extractionGate = make(chan struct{}, 1)

func Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case extractionGate <- struct{}{}:
		return func() { <-extractionGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func Identifier(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 500 {
		return "", errors.New(resources.Get("music.error.invalid_query"))
	}
	if strings.Contains(query, "://") {
		u, err := url.Parse(query)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
			return "", errors.New(resources.Get("music.error.invalid_url"))
		}
		host := strings.ToLower(u.Hostname())
		if host != "youtu.be" && host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com") && host != "soundcloud.com" && !strings.HasSuffix(host, ".soundcloud.com") && host != "bandcamp.com" && !strings.HasSuffix(host, ".bandcamp.com") {
			return "", errors.New(resources.Get("music.error.unsupported_host"))
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
				return "", errors.New(resources.Get("music.error.empty_query"))
			}
			return prefix + text, nil
		}
	}
	return "ytsearch:" + query, nil
}

type Metadata struct {
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
	Live       bool              `json:"is_live"`
	LiveStatus string            `json:"live_status"`
	Headers    map[string]string `json:"http_headers"`
	Entries    []Metadata        `json:"entries"`
}

func SearchTarget(identifier string, limit int) (string, string, error) {
	if strings.HasPrefix(identifier, "https://") {
		valid, err := Identifier(identifier)
		return valid, "", err
	}
	prefix, query, ok := strings.Cut(identifier, ":")
	if !ok || strings.TrimSpace(query) == "" {
		return "", "", errors.New(resources.Get("music.error.invalid_search"))
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
		return "", "", errors.New(resources.Get("music.error.unsupported_search"))
	}
}
func Search(ctx context.Context, identifier string, limit int, run func(context.Context, ...string) ([]byte, error), client *http.Client) ([]Track, error) {
	limit = max(1, min(limit, 20))
	target, source, err := SearchTarget(identifier, limit)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(identifier, "bcsearch:") {
		tracks, err := bandcamp(ctx, client, target)
		return tracks[:min(len(tracks), limit)], err
	}
	data, err := run(ctx, "--flat-playlist", "--playlist-end", fmt.Sprint(limit), "--dump-single-json", "--", target)
	if err != nil {
		return nil, err
	}
	var result Metadata
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	entries := result.Entries
	if entries == nil {
		entries = []Metadata{result}
	}
	tracks := make([]Track, 0, min(len(entries), limit))
	for _, entry := range entries[:min(len(entries), limit)] {
		if track, ok := FromMetadata(entry, source); ok {
			tracks = append(tracks, track)
		}
	}
	return tracks, nil
}
func FromMetadata(entry Metadata, source string) (Track, bool) {
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
	if _, err := Identifier(page); err != nil || !strings.HasPrefix(page, "https://") {
		return Track{}, false
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
	track := Track{ID: entry.ID, Title: clip(title, 200), Author: clip(author, 100), URL: page, Source: source, Live: entry.Live || entry.LiveStatus == "is_live" || entry.LiveStatus == "is_upcoming"}
	if entry.Duration > 0 && entry.Duration < 7*24*3600 {
		track.Duration = time.Duration(entry.Duration * float64(time.Second))
	}
	if u, err := url.Parse(art); err == nil && len(art) <= 2048 && u.Scheme == "https" && u.Host != "" && u.User == nil {
		track.ArtworkURL = art
	}
	return track, true
}

func clip(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit])
	}
	return s
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
func bandcamp(ctx context.Context, client *http.Client, target string) ([]Track, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; nyande music search)")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(resources.Get("music.error.bandcamp_status"), res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New(resources.Get("music.error.bandcamp_page_too_large"))
	}
	if bytes.Contains(data, []byte("Client Challenge")) {
		return nil, errors.New(resources.Get("music.error.bandcamp_challenge"))
	}
	return ParseBandcampSearch(data)
}
func ParseBandcampSearch(data []byte) ([]Track, error) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tracks := make([]Track, 0, 20)
	walkHTML(doc, func(n *html.Node) {
		if !htmlClass(n, "searchresult") || len(tracks) >= 20 {
			return
		}
		var entry Metadata
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
		if track, ok := FromMetadata(entry, "bandcamp"); ok {
			tracks = append(tracks, track)
		}
	})
	return tracks, nil
}
