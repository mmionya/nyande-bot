package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var twitterPathPattern = regexp.MustCompile(`(?i)^/([^/]+)/status/([0-9]+)`)

func (d *Downloader) downloadTwitter(ctx context.Context, value *url.URL) (Result, error) {
	match := twitterPathPattern.FindStringSubmatch(value.Path)
	if len(match) != 3 {
		return Result{}, errors.New("unsupported Twitter/X post URL")
	}
	endpoint := fmt.Sprintf("https://api.fxtwitter.com/%s/status/%s", url.PathEscape(match[1]), match[2])
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("User-Agent", "nyande-bot/2.0")
	response, err := d.client.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, fmt.Errorf("FxTwitter HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Result{}, fmt.Errorf("FxTwitter invalid JSON: %w", err)
	}
	tweet, _ := payload["tweet"].(map[string]any)
	caption, _ := tweet["text"].(string)
	media, _ := tweet["media"].(map[string]any)
	urls := make([]struct {
		url  string
		kind string
	}, 0)
	if videos, ok := media["videos"].([]any); ok {
		for _, raw := range videos {
			item, _ := raw.(map[string]any)
			link, _ := item["url"].(string)
			if link != "" {
				urls = append(urls, struct {
					url  string
					kind string
				}{link, "video"})
			}
		}
	}
	if photos, ok := media["photos"].([]any); ok {
		for _, raw := range photos {
			item, _ := raw.(map[string]any)
			link, _ := item["url"].(string)
			if link != "" {
				urls = append(urls, struct {
					url  string
					kind string
				}{link, "photo"})
			}
		}
	}
	if len(urls) == 0 {
		return Result{}, errors.New("FxTwitter returned no public media")
	}
	seen := map[string]bool{}
	items := make([]Media, 0, len(urls))
	for _, candidate := range urls {
		if seen[candidate.url] {
			continue
		}
		seen[candidate.url] = true
		parsed, err := url.Parse(candidate.url)
		if err != nil || parsed.Scheme != "https" || !hostMatches(parsed.Hostname(), "twimg.com") {
			continue
		}
		item, err := d.fetchMedia(ctx, parsed.String(), candidate.kind)
		if err != nil {
			return Result{}, err
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return Result{}, errors.New("FxTwitter returned no trusted media")
	}
	if author, ok := tweet["author"].(map[string]any); ok {
		name, _ := author["name"].(string)
		screen, _ := author["screen_name"].(string)
		byline := strings.TrimSpace(name)
		if screen != "" {
			byline = strings.TrimSpace(byline + " (@" + screen + ")")
		}
		if byline != "" {
			caption = strings.TrimSpace(caption)
			if caption != "" {
				caption += "\n\n"
			}
			caption += byline
		}
	}
	return Result{Items: items, Caption: cleanText(caption), Source: "twitter"}, nil
}
