package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const maxPinterestResponseSize = 10 * 1024 * 1024

var (
	pinterestDomains = []string{
		"pin.it",
		"pinterest.com", "pinterest.fr", "pinterest.de", "pinterest.ch",
		"pinterest.jp", "pinterest.cl", "pinterest.ca", "pinterest.it",
		"pinterest.co.uk", "pinterest.nz", "pinterest.ru", "pinterest.com.au",
		"pinterest.at", "pinterest.pt", "pinterest.co.kr", "pinterest.es",
		"pinterest.com.mx", "pinterest.dk", "pinterest.ph", "pinterest.th",
		"pinterest.com.uy", "pinterest.co", "pinterest.nl", "pinterest.info",
		"pinterest.kr", "pinterest.ie", "pinterest.vn", "pinterest.com.vn",
		"pinterest.ec", "pinterest.mx", "pinterest.in", "pinterest.pe",
		"pinterest.co.at", "pinterest.hu", "pinterest.co.in", "pinterest.co.nz",
		"pinterest.id", "pinterest.com.ec", "pinterest.com.py", "pinterest.tw",
		"pinterest.be", "pinterest.uk", "pinterest.com.bo", "pinterest.com.pe",
	}
	pinterestPinPathPattern = regexp.MustCompile(`(?i)^/pin/(?:[^/]*--)?([0-9]+)(?:/|$)`)
)

type pinterestMediaCandidate struct {
	kind string
	urls []string
}

type pinterestScoredURL struct {
	url   string
	score int64
}

func (d *Downloader) downloadPinterest(ctx context.Context, value *url.URL) (Result, error) {
	resolved, err := d.resolvePinterestURL(ctx, value)
	if err != nil {
		return d.downloadPinterestFallback(ctx, value, err)
	}
	pinID := pinterestPinID(resolved)
	if pinID == "" {
		return Result{}, errors.New("Pinterest URL does not contain a pin ID")
	}
	pin, err := d.fetchPinterestPin(ctx, pinID, resolved.String())
	if err != nil {
		return d.downloadPinterestFallback(ctx, resolved, err)
	}

	candidates := pinterestMediaCandidates(pin)
	items := make([]Media, 0, len(candidates))
	var combined error
	for _, candidate := range candidates {
		for _, raw := range candidate.urls {
			item, downloadErr := d.fetchMediaWithReferer(ctx, raw, candidate.kind, resolved.String())
			if downloadErr != nil {
				combined = errors.Join(combined, downloadErr)
				continue
			}
			items = append(items, item)
			break
		}
		if len(items) >= d.cfg.MaxMediaItems {
			break
		}
	}
	if len(items) == 0 {
		if combined == nil {
			combined = errors.New("Pinterest returned no downloadable media")
		}
		return d.downloadPinterestFallback(ctx, resolved, combined)
	}
	return Result{Items: items, Caption: pinterestCaption(pin), Source: "pinterest"}, nil
}

func (d *Downloader) downloadPinterestFallback(ctx context.Context, value *url.URL, nativeErr error) (Result, error) {
	result, err := d.downloadYTDLP(ctx, value)
	if err == nil {
		result.Source = "pinterest"
		return result, nil
	}
	return Result{}, fmt.Errorf("Pinterest download failed: %w", errors.Join(nativeErr, err))
}

func (d *Downloader) resolvePinterestURL(ctx context.Context, value *url.URL) (*url.URL, error) {
	if !hostMatches(value.Hostname(), "pin.it") {
		return value, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, value.String(), nil)
	if err != nil {
		return value, err
	}
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	response, err := d.client.Do(request)
	if err != nil {
		return value, err
	}
	defer response.Body.Close()
	resolved := response.Request.URL
	if !IsPinterest(resolved) || hostMatches(resolved.Hostname(), "pin.it") {
		return value, errors.New("Pinterest short link redirected outside Pinterest")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return resolved, fmt.Errorf("Pinterest short link HTTP %d", response.StatusCode)
	}
	return resolved, nil
}

func pinterestPinID(value *url.URL) string {
	match := pinterestPinPathPattern.FindStringSubmatch(value.Path)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func (d *Downloader) fetchPinterestPin(ctx context.Context, pinID, referer string) (map[string]any, error) {
	options, _ := json.Marshal(map[string]any{
		"options": map[string]string{
			"field_set_key": "unauth_react_main_pin",
			"id":            pinID,
		},
	})
	endpoint, _ := url.Parse("https://www.pinterest.com/resource/PinResource/get/")
	query := endpoint.Query()
	query.Set("data", string(options))
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	request.Header.Set("Referer", referer)
	request.Header.Set("X-Pinterest-PWS-Handler", "www/[username].js")
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if !hostMatches(response.Request.URL.Hostname(), "pinterest.com") {
		return nil, errors.New("Pinterest API redirected outside Pinterest")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Pinterest API HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPinterestResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPinterestResponseSize {
		return nil, errors.New("Pinterest API response is too large")
	}
	return pinterestPinFromResponse(data)
}

func pinterestPinFromResponse(data []byte) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("Pinterest API returned invalid JSON: %w", err)
	}
	resource, _ := payload["resource_response"].(map[string]any)
	pin, _ := resource["data"].(map[string]any)
	if len(pin) == 0 {
		return nil, errors.New("Pinterest API returned no public pin data")
	}
	return pin, nil
}

func pinterestMediaCandidates(pin map[string]any) []pinterestMediaCandidate {
	if carousel, ok := pin["carousel_data"].(map[string]any); ok {
		if slots, ok := carousel["carousel_slots"].([]any); ok {
			if result := pinterestCandidatesFromList(slots); len(result) > 0 {
				return result
			}
		}
	}
	if story, ok := pin["story_pin_data"].(map[string]any); ok {
		if pages, ok := story["pages"].([]any); ok {
			result := make([]pinterestMediaCandidate, 0, len(pages))
			for _, rawPage := range pages {
				page, _ := rawPage.(map[string]any)
				blocks, _ := page["blocks"].([]any)
				if len(blocks) == 0 {
					if candidate, ok := pinterestCandidateFromContainer(page); ok {
						result = append(result, candidate)
					}
					continue
				}
				result = append(result, pinterestCandidatesFromList(blocks)...)
			}
			if len(result) > 0 {
				return result
			}
		}
	}
	if candidate, ok := pinterestCandidateFromContainer(pin); ok {
		return []pinterestMediaCandidate{candidate}
	}
	return nil
}

func pinterestCandidatesFromList(values []any) []pinterestMediaCandidate {
	result := make([]pinterestMediaCandidate, 0, len(values))
	for _, raw := range values {
		container, _ := raw.(map[string]any)
		if candidate, ok := pinterestCandidateFromContainer(container); ok {
			result = append(result, candidate)
		}
	}
	return result
}

func pinterestCandidateFromContainer(container map[string]any) (pinterestMediaCandidate, bool) {
	if urls := pinterestVideoURLs(container); len(urls) > 0 {
		return pinterestMediaCandidate{kind: "video", urls: urls}, true
	}
	if pinterestContainsVideo(container) {
		return pinterestMediaCandidate{kind: "video"}, true
	}
	if urls := pinterestImageURLs(container); len(urls) > 0 {
		return pinterestMediaCandidate{kind: "photo", urls: urls}, true
	}
	return pinterestMediaCandidate{}, false
}

func pinterestContainsVideo(value any) bool {
	switch current := value.(type) {
	case []any:
		for _, child := range current {
			if pinterestContainsVideo(child) {
				return true
			}
		}
	case map[string]any:
		if videoList, ok := current["video_list"].(map[string]any); ok && len(videoList) > 0 {
			return true
		}
		for _, child := range current {
			if pinterestContainsVideo(child) {
				return true
			}
		}
	}
	return false
}

func pinterestVideoURLs(container map[string]any) []string {
	candidates := make([]pinterestScoredURL, 0)
	var walk func(any)
	walk = func(value any) {
		switch current := value.(type) {
		case []any:
			for _, child := range current {
				walk(child)
			}
		case map[string]any:
			if videoList, ok := current["video_list"].(map[string]any); ok {
				for _, rawFormat := range videoList {
					format, _ := rawFormat.(map[string]any)
					raw, _ := format["url"].(string)
					if normalized := trustedPinterestMediaURL(raw); normalized != "" && !isHLSURL(normalized) {
						score := int64FromJSON(format["width"])*int64FromJSON(format["height"])*1_000_000 +
							int64FromJSON(format["bitrate"])
						candidates = append(candidates, pinterestScoredURL{url: normalized, score: score})
					}
				}
			}
			for _, child := range current {
				walk(child)
			}
		}
	}
	walk(container)
	return sortedPinterestURLs(candidates)
}

func pinterestImageURLs(container map[string]any) []string {
	candidates := make([]pinterestScoredURL, 0)
	var walk func(any, bool, string)
	walk = func(value any, insideImages bool, variant string) {
		switch current := value.(type) {
		case []any:
			for _, child := range current {
				walk(child, insideImages, variant)
			}
		case map[string]any:
			if raw, ok := current["url"].(string); ok && insideImages {
				if normalized := trustedPinterestMediaURL(raw); normalized != "" {
					score := int64FromJSON(current["width"]) * int64FromJSON(current["height"])
					if strings.Contains(strings.ToLower(variant), "orig") {
						score += 1 << 60
					}
					candidates = append(candidates, pinterestScoredURL{url: normalized, score: score})
				}
			}
			for childKey, child := range current {
				childInsideImages := insideImages || childKey == "images"
				childVariant := variant
				if insideImages || childKey == "images" {
					childVariant = childKey
				}
				walk(child, childInsideImages, childVariant)
			}
		}
	}
	walk(container, false, "")
	return sortedPinterestURLs(candidates)
}

func sortedPinterestURLs(candidates []pinterestScoredURL) []string {
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	seen := make(map[string]bool)
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if !seen[candidate.url] {
			seen[candidate.url] = true
			result = append(result, candidate.url)
		}
	}
	return result
}

func trustedPinterestMediaURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(strings.ReplaceAll(raw, `\u0026`, "&")))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		!hostMatches(parsed.Hostname(), "pinimg.com") {
		return ""
	}
	return parsed.String()
}

func isHLSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8")
}

func pinterestCaption(pin map[string]any) string {
	title := cleanText(firstMapString(pin, "title", "grid_title"))
	description := cleanText(firstMapString(pin, "seo_description", "description"))
	parts := make([]string, 0, 3)
	if title != "" {
		parts = append(parts, title)
	}
	if description != "" && description != title {
		parts = append(parts, description)
	}
	for _, key := range []string{"closeup_attribution", "pinner"} {
		if authorData, ok := pin[key].(map[string]any); ok {
			if author := cleanText(firstMapString(authorData, "full_name", "username")); author != "" {
				parts = append(parts, author)
				break
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
