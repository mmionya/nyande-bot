package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hyphentae/nyande-bot/internal/config"
)

type Media struct {
	Kind string
	Name string
	MIME string
	Data []byte
	URL  string
}

type Result struct {
	Items   []Media
	Caption string
	Source  string
}

type Downloader struct {
	cfg        config.Config
	client     *http.Client
	apiMu      sync.Mutex
	lastTikTok time.Time
}

func New(cfg config.Config) *Downloader {
	return &Downloader{
		cfg: cfg,
		client: &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 6 {
					return errors.New("too many redirects")
				}
				if request.URL.Scheme != "https" && request.URL.Scheme != "http" {
					return errors.New("redirected to unsupported URL scheme")
				}
				return nil
			},
		},
	}
}

func (d *Downloader) Download(ctx context.Context, rawURL string) (Result, error) {
	parsed, err := normalizedURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	var operation func(context.Context, *url.URL) (Result, error)
	switch {
	case IsTikTok(parsed):
		operation = d.downloadTikTok
	case IsInstagram(parsed):
		operation = d.downloadInstagram
	case IsTwitter(parsed):
		operation = d.downloadTwitter
	case IsReddit(parsed):
		operation = d.downloadReddit
	case IsXiaohongshu(parsed):
		operation = d.downloadXiaohongshu
	case IsPinterest(parsed):
		operation = d.downloadPinterest
	case IsYouTube(parsed):
		operation = d.downloadYTDLP
	case isDirectMedia(parsed):
		operation = d.downloadDirect
	default:
		return Result{}, errors.New("unsupported media URL")
	}

	var result Result
	for attempt := 1; attempt <= d.cfg.RetryAttempts; attempt++ {
		result, err = operation(ctx, parsed)
		if err == nil {
			if len(result.Items) > d.cfg.MaxMediaItems {
				result.Items = result.Items[:d.cfg.MaxMediaItems]
			}
			result.Caption = truncateCaption(result.Caption)
			return result, nil
		}
		if attempt < d.cfg.RetryAttempts && retryable(err) {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(d.cfg.RetryDelay * time.Duration(attempt)):
			}
		}
	}
	return Result{}, err
}

func Supported(raw string) bool {
	parsed, err := normalizedURL(raw)
	if err != nil {
		return false
	}
	return IsTikTok(parsed) || IsInstagram(parsed) || IsTwitter(parsed) ||
		IsReddit(parsed) || IsXiaohongshu(parsed) || IsPinterest(parsed) ||
		IsYouTube(parsed) || isDirectMedia(parsed)
}

func AllowedHost(raw string) bool {
	parsed, err := normalizedURL(raw)
	if err != nil {
		return false
	}
	return Supported(raw) || isKnownMediaHost(parsed.Hostname())
}

func IsTikTok(value *url.URL) bool {
	return hostMatches(value.Hostname(), "tiktok.com", "vm.tiktok.com", "vt.tiktok.com")
}

func IsInstagram(value *url.URL) bool {
	return hostMatches(value.Hostname(), "instagram.com", "instagr.am")
}

func IsTwitter(value *url.URL) bool {
	return hostMatches(value.Hostname(), "twitter.com", "x.com", "mobile.twitter.com")
}

func IsReddit(value *url.URL) bool {
	return hostMatches(value.Hostname(), "reddit.com", "redd.it", "v.redd.it")
}

func IsXiaohongshu(value *url.URL) bool {
	return hostMatches(value.Hostname(), "xiaohongshu.com", "xhslink.com")
}

func IsPinterest(value *url.URL) bool {
	return hostMatches(value.Hostname(), pinterestDomains...)
}

func IsYouTube(value *url.URL) bool {
	return hostMatches(value.Hostname(), "youtube.com", "youtu.be", "youtube-nocookie.com")
}

func normalizedURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(strings.TrimRight(raw, ".,;:!?)]}"))
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
		return nil, errors.New("invalid HTTP URL")
	}
	return parsed, nil
}

func hostMatches(host string, allowed ...string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, candidate := range allowed {
		candidate = strings.ToLower(candidate)
		if host == candidate || strings.HasSuffix(host, "."+candidate) {
			return true
		}
	}
	return false
}

func isDirectMedia(value *url.URL) bool {
	extension := strings.ToLower(path.Ext(value.Path))
	switch extension {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".mp4", ".mov", ".webm":
		return true
	default:
		return false
	}
}

func isKnownMediaHost(host string) bool {
	return hostMatches(host,
		"cdninstagram.com", "fbcdn.net", "tiktokcdn.com", "tiktokcdn-us.com",
		"twimg.com", "redditmedia.com", "redd.it", "reddit.com", "googlevideo.com",
		"xhscdn.com", "pinimg.com",
	)
}

func (d *Downloader) downloadDirect(ctx context.Context, value *url.URL) (Result, error) {
	if !isPublicURL(value) {
		return Result{}, errors.New("direct media URL resolves to a private address")
	}
	item, err := d.fetchMedia(ctx, value.String(), "")
	if err != nil {
		return Result{}, err
	}
	return Result{Items: []Media{item}, Source: "direct"}, nil
}

func (d *Downloader) fetchMedia(ctx context.Context, rawURL, expectedKind string) (Media, error) {
	return d.fetchMediaWithReferer(ctx, rawURL, expectedKind, "")
}

func (d *Downloader) fetchMediaWithReferer(ctx context.Context, rawURL, expectedKind, referer string) (Media, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !isPublicURL(parsed) {
		return Media{}, errors.New("media URL does not resolve to a public address")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Media{}, err
	}
	request.Header.Set("User-Agent", browserUserAgent)
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return Media{}, err
	}
	defer response.Body.Close()
	if !isPublicURL(response.Request.URL) {
		return Media{}, errors.New("media redirected to a non-public address")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Media{}, fmt.Errorf("media HTTP %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	kind := kindFromContentType(contentType)
	if kind == "" {
		kind = kindFromExtension(response.Request.URL.Path)
	}
	if kind == "" {
		return Media{}, fmt.Errorf("unsupported media content type %q", contentType)
	}
	if expectedKind != "" && expectedKind != kind {
		return Media{}, fmt.Errorf("expected %s, received %s", expectedKind, kind)
	}
	if response.ContentLength > d.cfg.MaxFileSize {
		return Media{}, fmt.Errorf("media is too large: %d bytes", response.ContentLength)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, d.cfg.MaxFileSize+1))
	if err != nil {
		return Media{}, err
	}
	if int64(len(data)) > d.cfg.MaxFileSize {
		return Media{}, fmt.Errorf("media exceeds %d bytes", d.cfg.MaxFileSize)
	}
	name := filenameFromURL(response.Request.URL, contentType)
	return Media{Kind: kind, Name: name, MIME: contentType, Data: data}, nil
}

func kindFromContentType(value string) string {
	switch {
	case strings.HasPrefix(value, "image/gif"):
		return "animation"
	case strings.HasPrefix(value, "image/"):
		return "photo"
	case strings.HasPrefix(value, "video/"):
		return "video"
	default:
		return ""
	}
}

func kindFromExtension(value string) string {
	switch strings.ToLower(path.Ext(value)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return "photo"
	case ".gif":
		return "animation"
	case ".mp4", ".mov", ".webm":
		return "video"
	default:
		return ""
	}
}

func filenameFromURL(value *url.URL, contentType string) string {
	name := path.Base(value.Path)
	if name == "" || name == "." || name == "/" {
		extensions, _ := mime.ExtensionsByType(contentType)
		extension := ".bin"
		if len(extensions) > 0 {
			extension = extensions[0]
		}
		return "media" + extension
	}
	if len(name) > 120 {
		extension := path.Ext(name)
		name = name[:100] + extension
	}
	return name
}

func isPublicURL(value *url.URL) bool {
	host := value.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return false
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if address.IsLoopback() || address.IsPrivate() || address.IsUnspecified() || address.IsMulticast() {
			return false
		}
	}
	return true
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "timeout") || strings.Contains(text, "temporar") ||
		strings.Contains(text, "connection") || strings.Contains(text, "http 429") ||
		strings.Contains(text, "http 5")
}

var whitespace = regexp.MustCompile(`\s+`)

func cleanText(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "&amp;", "&"), "&#39;", "'"))
	value = strings.ReplaceAll(strings.ReplaceAll(value, "&quot;", `"`), "&lt;", "<")
	value = strings.ReplaceAll(value, "&gt;", ">")
	return whitespace.ReplaceAllString(value, " ")
}

func truncateCaption(value string) string {
	value = strings.TrimSpace(value)
	const limit = 1024
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131 Safari/537.36"
