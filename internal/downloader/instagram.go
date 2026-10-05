package downloader

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

//go:embed instagram_photos.py
var instagramPhotosPlugin []byte

var (
	instagramShortcode       = regexp.MustCompile(`(?i)/(p|reel|reels|tv)/([A-Za-z0-9_-]+)`)
	metaTagPattern           = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	metaProperty             = regexp.MustCompile(`(?i)(?:property|name)\s*=\s*["']([^"']+)["']`)
	metaContent              = regexp.MustCompile(`(?i)content\s*=\s*["']([^"']*)["']`)
	videoURLPattern          = regexp.MustCompile(`(?s)video_url\\*"\s*:\s*\\*"(.*?)\\*"`)
	displayURLPattern        = regexp.MustCompile(`(?s)display_url\\*"\s*:\s*\\*"(.*?)\\*"`)
	instagramCaptionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?s)caption_text\\*"\s*:\s*\\*"(.*?)\\*"`),
		regexp.MustCompile(`(?s)edge_media_to_caption.*?text\\*"\s*:\s*\\*"(.*?)\\*"`),
		regexp.MustCompile(`(?s)caption\\*"\s*:\s*\{.*?text\\*"\s*:\s*\\*"(.*?)\\*"`),
	}
)

type instagramApifyInput struct {
	URL []string `json:"url"`
}

func (d *Downloader) downloadInstagram(ctx context.Context, value *url.URL) (Result, error) {
	match := instagramShortcode.FindStringSubmatch(value.Path)
	if len(match) != 3 {
		return Result{}, errors.New("Instagram URL does not contain a post or Reel shortcode")
	}
	mediaType := strings.ToLower(match[1])
	if mediaType == "reels" {
		mediaType = "reel"
	}
	canonical := "https://www.instagram.com/" + mediaType + "/" + match[2] + "/"
	embed := canonical + "embed/captioned/"
	var combined error
	caption := ""
	canonicalURL, _ := url.Parse(canonical)
	if mediaType == "p" {
		// The extractor retains carousel order and distinguishes photos from video covers.
		result, err := d.downloadYTDLPWithOptions(ctx, canonicalURL, true)
		if err == nil {
			return result, nil
		}
		combined = err
	}
	mediaCandidates := make([][]string, 0, 2)
	for _, pageURL := range []string{canonical, embed} {
		page, err := d.fetchInstagramPage(ctx, pageURL)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		urls, pageCaption := instagramPageMedia(page, mediaType == "p")
		caption = firstNonEmpty(caption, pageCaption)
		if len(urls) > 0 {
			mediaCandidates = append(mediaCandidates, urls)
		}
	}
	for _, urls := range mediaCandidates {
		items, err := d.downloadInstagramItems(ctx, urls, canonical)
		if err == nil && len(items) > 0 {
			return Result{Items: items, Caption: caption, Source: "instagram"}, nil
		}
		combined = errors.Join(combined, err)
	}

	if mediaType != "p" {
		result, err := d.downloadYTDLPWithOptions(ctx, canonicalURL, true)
		if err == nil {
			result.Source = "instagram"
			if result.Caption == "" {
				result.Caption = caption
			}
			return result, nil
		}
		combined = errors.Join(combined, err)
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if mediaType == "reel" || mediaType == "tv" {
		item, err := d.downloadInstagramVX(ctx, match[2], canonical)
		if err == nil {
			return Result{Items: []Media{item}, Caption: caption, Source: "instagram"}, nil
		}
		combined = errors.Join(combined, fmt.Errorf("vxinstagram download failed: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if d.cfg.APIFYToken != "" {
		result, err := d.downloadInstagramApify(ctx, canonical)
		if err == nil {
			if result.Caption == "" {
				result.Caption = caption
			}
			return result, nil
		}
		combined = errors.Join(combined, err)
	}
	return Result{}, fmt.Errorf("instagram download failed: %w", combined)
}

func (d *Downloader) downloadInstagramVX(ctx context.Context, shortcode, referer string) (Media, error) {
	client := *d.client
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || !hostMatches(next.URL.Hostname(),
			"vxinstagram.com", "cdninstagram.com", "fbcdn.net", "snapcdn.app", "rapidcdn.app") {
			return errors.New("vxinstagram redirected to an unsupported media host")
		}
		if len(via) >= 6 {
			return errors.New("too many vxinstagram redirects")
		}
		if d.client.CheckRedirect != nil {
			return d.client.CheckRedirect(next, via)
		}
		return nil
	}
	mediaDownloader := Downloader{cfg: d.cfg, client: &client}
	// The documented offload route returns one item, so use it only for Reels/TV.
	return mediaDownloader.fetchMediaWithReferer(ctx,
		"https://vxinstagram.com/offload/"+url.PathEscape(shortcode), "video", referer)
}

func (d *Downloader) fetchInstagramPage(ctx context.Context, pageURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	response, err := d.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if !IsInstagram(response.Request.URL) {
		return "", errors.New("Instagram redirected to an unsupported host")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Instagram page HTTP %d", response.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(response.Body, 5*1024*1024))
	if err != nil {
		return "", err
	}
	return string(page), nil
}

func instagramPageMedia(page string, allowImages bool) ([]string, string) {
	caption := firstNonEmpty(metaValue(page, "og:description"), metaValue(page, "description"))
	caption = cleanInstagramDescription(html.UnescapeString(caption))
	if caption == "" {
		for _, pattern := range instagramCaptionPatterns {
			if match := pattern.FindStringSubmatch(page); len(match) == 2 {
				caption = cleanText(decodeInstagramJSONString(match[1]))
				if caption != "" {
					break
				}
			}
		}
	}

	videos := make([]string, 0)
	for _, property := range []string{"og:video:secure_url", "og:video", "og:image"} {
		if value := metaValue(page, property); value != "" {
			if property == "og:image" {
				continue
			}
			videos = append(videos, html.UnescapeString(value))
		}
	}
	videos = append(videos, instagramJSONURLs(page, videoURLPattern)...)
	if videos = uniqueHTTPS(videos); len(videos) > 0 {
		return videos, caption
	}
	if !allowImages || strings.Contains(strings.ToLower(metaValue(page, "og:type")), "video") {
		return nil, caption
	}

	images := instagramJSONURLs(page, displayURLPattern)
	if len(images) == 0 {
		if value := metaValue(page, "og:image"); value != "" {
			images = append(images, html.UnescapeString(value))
		}
	}
	return uniqueHTTPS(images), caption
}

func instagramJSONURLs(page string, pattern *regexp.Regexp) []string {
	values := make([]string, 0)
	for _, match := range pattern.FindAllStringSubmatch(page, -1) {
		if len(match) != 2 {
			continue
		}
		if decoded := decodeInstagramJSONString(match[1]); decoded != "" {
			values = append(values, decoded)
		}
	}
	return values
}

func decodeInstagramJSONString(value string) string {
	decoded := value
	for range 4 {
		var next string
		if json.Unmarshal([]byte(`"`+decoded+`"`), &next) != nil || next == decoded {
			break
		}
		decoded = next
	}
	decoded = strings.ReplaceAll(decoded, `\/`, `/`)
	return html.UnescapeString(strings.TrimSpace(decoded))
}

func metaValue(page, requested string) string {
	for _, tag := range metaTagPattern.FindAllString(page, -1) {
		property := metaProperty.FindStringSubmatch(tag)
		content := metaContent.FindStringSubmatch(tag)
		if len(property) == 2 && len(content) == 2 && strings.EqualFold(property[1], requested) {
			return content[1]
		}
	}
	return ""
}

func cleanInstagramDescription(value string) string {
	value = strings.TrimSpace(value)
	if quote := strings.Index(value, `: "`); quote >= 0 && strings.HasSuffix(value, `"`) {
		return strings.TrimSuffix(value[quote+3:], `"`)
	}
	return cleanText(value)
}

func (d *Downloader) downloadInstagramApify(ctx context.Context, postURL string) (Result, error) {
	if d.cfg.APIFYToken == "" {
		return Result{}, errors.New("Instagram hid this post; APIFY_TOKEN is required for the fallback")
	}
	actor := strings.ReplaceAll(d.cfg.APIFYInstagramActor, "/", "~")
	endpoint := fmt.Sprintf(
		"https://api.apify.com/v2/acts/%s/run-sync-get-dataset-items?format=json&clean=true&maxItems=1",
		url.PathEscape(actor),
	)
	input := instagramApifyInput{URL: []string{postURL}}
	data, _ := json.Marshal(input)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+d.cfg.APIFYToken)
	response, err := d.client.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return Result{}, fmt.Errorf("Apify HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	responseData, err := io.ReadAll(io.LimitReader(response.Body, 10*1024*1024))
	if err != nil {
		return Result{}, err
	}
	var items []map[string]any
	if err := json.Unmarshal(responseData, &items); err != nil {
		var item map[string]any
		if singleErr := json.Unmarshal(responseData, &item); singleErr != nil {
			return Result{}, err
		}
		items = []map[string]any{item}
	}
	if len(items) == 0 {
		return Result{}, errors.New("Apify returned no Instagram results")
	}
	mediaURLs := collectApifyMedia(items[0])
	if len(mediaURLs) == 0 {
		return Result{}, errors.New("Apify returned no Instagram media URLs")
	}
	downloaded, err := d.downloadInstagramItems(ctx, mediaURLs, postURL)
	if err != nil {
		return Result{}, err
	}
	caption := firstMapString(items[0], "caption", "text", "description", "alt")
	return Result{Items: downloaded, Caption: cleanText(caption), Source: "instagram"}, nil
}

func collectApifyMedia(item map[string]any) []string {
	if result, ok := item["result"].([]any); ok {
		resolved := make([]string, 0, len(result))
		for _, value := range result {
			entry, ok := value.(map[string]any)
			if !ok {
				continue
			}
			raw, _ := entry["url"].(string)
			if strings.HasPrefix(raw, "https://") {
				resolved = append(resolved, raw)
			}
		}
		if resolved = uniqueHTTPS(resolved); len(resolved) > 0 {
			return resolved
		}
	}

	type candidate struct {
		priority int
		value    string
	}
	candidates := make([]candidate, 0)
	var walk func(any, string)
	walk = func(value any, key string) {
		switch current := value.(type) {
		case map[string]any:
			for childKey, child := range current {
				walk(child, strings.ToLower(childKey))
			}
		case []any:
			for _, child := range current {
				walk(child, key)
			}
		case string:
			if !strings.HasPrefix(current, "https://") {
				return
			}
			lower := strings.ToLower(key)
			for _, skipped := range []string{
				"thumbnail", "thumb", "avatar", "profile", "icon", "logo",
				"permalink", "shortcode", "posturl", "pageurl", "inputurl",
			} {
				if strings.Contains(lower, skipped) {
					return
				}
			}
			priority := 0
			switch {
			case strings.Contains(lower, "video"):
				priority = 4
			case strings.Contains(lower, "image") || strings.Contains(lower, "photo") || strings.Contains(lower, "display"):
				priority = 3
			case strings.Contains(lower, "download") || strings.Contains(lower, "media") || strings.Contains(lower, "src"):
				priority = 2
			case lower == "url":
				priority = 1
			}
			if priority > 0 {
				candidates = append(candidates, candidate{priority, current})
			}
		}
	}
	walk(item, "")
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].priority > candidates[j].priority })
	raw := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		raw = append(raw, candidate.value)
	}
	return uniqueHTTPS(raw)
}

func (d *Downloader) downloadInstagramItems(ctx context.Context, rawURLs []string, referer string) ([]Media, error) {
	items := make([]Media, 0, len(rawURLs))
	var combined error
	for _, raw := range rawURLs {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" {
			continue
		}
		if !instagramMediaHost(parsed.Hostname()) {
			continue
		}
		item, err := d.fetchMediaWithReferer(ctx, parsed.String(), "", referer)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		items = append(items, item)
		if len(items) >= d.cfg.MaxMediaItems {
			break
		}
	}
	if len(items) == 0 {
		if combined != nil {
			return nil, combined
		}
		return nil, errors.New("Instagram media could not be downloaded")
	}
	return items, nil
}

func instagramMediaHost(host string) bool {
	return hostMatches(
		host,
		"cdninstagram.com",
		"fbcdn.net",
		"instagram.com",
		"snapcdn.app",
		"apify.com",
		"apifyusercontent.com",
		"amazonaws.com",
	)
}

func uniqueHTTPS(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ReplaceAll(strings.TrimSpace(value), `\u0026`, "&")
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func firstMapString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result, ok := value[key].(string); ok && strings.TrimSpace(result) != "" {
			return result
		}
	}
	return ""
}
