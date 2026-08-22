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

const maxXiaohongshuPageSize = 8 * 1024 * 1024

var xiaohongshuNotePathPattern = regexp.MustCompile(`(?i)^/(?:explore|discovery/item)/([0-9a-f]+)`)

type xiaohongshuVideoCandidate struct {
	url   string
	score int64
}

func (d *Downloader) downloadXiaohongshu(ctx context.Context, value *url.URL) (Result, error) {
	fallbackURL := value
	page, resolved, err := d.fetchXiaohongshuPage(ctx, value)
	if err != nil {
		if recovered := recoveredXiaohongshuPostURL(resolved); recovered != nil {
			fallbackURL = recovered
			recoveredPage, recoveredURL, recoveredErr := d.fetchXiaohongshuPage(ctx, recovered)
			if recoveredErr == nil {
				page, resolved, err = recoveredPage, recoveredURL, nil
			} else {
				resolved = recovered
				err = errors.Join(err, recoveredErr)
			}
		}
	}
	if err != nil {
		return d.downloadXiaohongshuFallback(ctx, fallbackURL, err)
	}
	note, err := xiaohongshuNoteFromPage(page, xiaohongshuNoteID(resolved))
	if err != nil {
		return d.downloadXiaohongshuFallback(ctx, resolved, err)
	}
	caption := xiaohongshuCaption(note)

	video, isVideoNote := note["video"].(map[string]any)
	if isVideoNote && len(video) > 0 {
		videoURLs := xiaohongshuVideoURLs(note)
		if len(videoURLs) == 0 {
			return d.downloadXiaohongshuFallback(ctx, resolved, errors.New("Xiaohongshu returned no public video URL"))
		}
		var combined error
		for _, raw := range videoURLs {
			item, downloadErr := d.fetchMediaWithReferer(ctx, raw, "video", resolved.String())
			if downloadErr == nil {
				return Result{Items: []Media{item}, Caption: caption, Source: "xiaohongshu"}, nil
			}
			combined = errors.Join(combined, downloadErr)
		}
		return d.downloadXiaohongshuFallback(ctx, resolved, combined)
	}

	imageURLs := xiaohongshuImageURLs(note)
	items := make([]Media, 0, len(imageURLs))
	var combined error
	for _, alternatives := range imageURLs {
		for _, raw := range alternatives {
			item, downloadErr := d.fetchMediaWithReferer(ctx, raw, "photo", resolved.String())
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
			combined = errors.New("Xiaohongshu returned no downloadable media")
		}
		return Result{}, combined
	}
	return Result{Items: items, Caption: caption, Source: "xiaohongshu"}, nil
}

func (d *Downloader) downloadXiaohongshuFallback(ctx context.Context, value *url.URL, nativeErr error) (Result, error) {
	result, err := d.downloadYTDLP(ctx, value)
	if err == nil {
		result.Source = "xiaohongshu"
		return result, nil
	}
	return Result{}, fmt.Errorf("Xiaohongshu download failed: %w", errors.Join(nativeErr, err))
}

func (d *Downloader) fetchXiaohongshuPage(ctx context.Context, value *url.URL) (string, *url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, value.String(), nil)
	if err != nil {
		return "", value, err
	}
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Referer", "https://www.xiaohongshu.com/")
	response, err := d.client.Do(request)
	if err != nil {
		return "", value, err
	}
	defer response.Body.Close()
	resolved := response.Request.URL
	if !IsXiaohongshu(resolved) || hostMatches(resolved.Hostname(), "xhslink.com") {
		return "", resolved, errors.New("Xiaohongshu link redirected outside a post page")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", resolved, fmt.Errorf("Xiaohongshu page HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxXiaohongshuPageSize+1))
	if err != nil {
		return "", resolved, err
	}
	if len(data) > maxXiaohongshuPageSize {
		return "", resolved, errors.New("Xiaohongshu page is too large")
	}
	return string(data), resolved, nil
}

func xiaohongshuNoteID(value *url.URL) string {
	match := xiaohongshuNotePathPattern.FindStringSubmatch(value.Path)
	if len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return firstNonEmpty(value.Query().Get("noteId"), value.Query().Get("note_id"))
}

func recoveredXiaohongshuPostURL(value *url.URL) *url.URL {
	if value == nil || !IsXiaohongshu(value) || xiaohongshuNotePathPattern.MatchString(value.Path) {
		return nil
	}
	noteID := strings.ToLower(xiaohongshuNoteID(value))
	if matched, _ := regexp.MatchString(`^[0-9a-f]+$`, noteID); !matched {
		return nil
	}
	recovered := *value
	recovered.Scheme = "https"
	recovered.Host = "www.xiaohongshu.com"
	recovered.Path = "/explore/" + noteID
	recovered.RawPath = ""
	recovered.Fragment = ""
	query := recovered.Query()
	for _, key := range []string{"noteId", "note_id", "source", "errorCode"} {
		query.Del(key)
	}
	recovered.RawQuery = query.Encode()
	return &recovered
}

func xiaohongshuNoteFromPage(page, noteID string) (map[string]any, error) {
	raw, err := extractAssignedJSONObject(page, "window.__INITIAL_STATE__")
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(replaceJavaScriptUndefined(raw)), &state); err != nil {
		return nil, fmt.Errorf("invalid Xiaohongshu initial state: %w", err)
	}
	noteRoot, _ := state["note"].(map[string]any)
	detailMap, _ := noteRoot["noteDetailMap"].(map[string]any)
	if noteID != "" {
		if detail, ok := detailMap[noteID].(map[string]any); ok {
			if note, ok := detail["note"].(map[string]any); ok {
				return note, nil
			}
		}
	}
	for _, rawDetail := range detailMap {
		detail, _ := rawDetail.(map[string]any)
		if note, ok := detail["note"].(map[string]any); ok {
			return note, nil
		}
	}
	return nil, errors.New("Xiaohongshu page did not contain public note data")
}

func extractAssignedJSONObject(page, variable string) (string, error) {
	start := strings.Index(page, variable)
	if start < 0 {
		return "", errors.New("Xiaohongshu page did not contain initial state")
	}
	start += len(variable)
	if equals := strings.IndexByte(page[start:], '='); equals >= 0 {
		start += equals + 1
	} else {
		return "", errors.New("Xiaohongshu initial state assignment is malformed")
	}
	for start < len(page) && (page[start] == ' ' || page[start] == '\t' || page[start] == '\r' || page[start] == '\n') {
		start++
	}
	if start >= len(page) || page[start] != '{' {
		return "", errors.New("Xiaohongshu initial state is not an object")
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(page); index++ {
		character := page[index]
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return page[start : index+1], nil
			}
		}
	}
	return "", errors.New("Xiaohongshu initial state object is incomplete")
}

func replaceJavaScriptUndefined(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	inString := false
	escaped := false
	for index := 0; index < len(value); {
		character := value[index]
		if inString {
			result.WriteByte(character)
			index++
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		if character == '"' {
			inString = true
			result.WriteByte(character)
			index++
			continue
		}
		if strings.HasPrefix(value[index:], "undefined") &&
			(index == 0 || !isJavaScriptIdentifierByte(value[index-1])) &&
			(index+len("undefined") == len(value) || !isJavaScriptIdentifierByte(value[index+len("undefined")])) {
			result.WriteString("null")
			index += len("undefined")
			continue
		}
		result.WriteByte(character)
		index++
	}
	return result.String()
}

func isJavaScriptIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func xiaohongshuVideoURLs(note map[string]any) []string {
	video, _ := note["video"].(map[string]any)
	if len(video) == 0 {
		return nil
	}
	candidates := make([]xiaohongshuVideoCandidate, 0)
	if consumer, ok := video["consumer"].(map[string]any); ok {
		if key, ok := consumer["originVideoKey"].(string); ok && strings.TrimSpace(key) != "" {
			candidates = append(candidates, xiaohongshuVideoCandidate{
				url: "https://sns-video-bd.xhscdn.com/" + strings.TrimLeft(key, "/"), score: 1<<62 - 1,
			})
		}
	}
	media, _ := video["media"].(map[string]any)
	collectXiaohongshuStreams(media["stream"], &candidates)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	seen := make(map[string]bool)
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if normalized := trustedXiaohongshuMediaURL(candidate.url); normalized != "" && !seen[normalized] {
			seen[normalized] = true
			result = append(result, normalized)
		}
	}
	return result
}

func collectXiaohongshuStreams(value any, candidates *[]xiaohongshuVideoCandidate) {
	switch current := value.(type) {
	case []any:
		for _, child := range current {
			collectXiaohongshuStreams(child, candidates)
		}
	case map[string]any:
		score := int64FromJSON(current["width"])*int64FromJSON(current["height"])*1_000_000 +
			int64FromJSON(current["avgBitrate"])
		if raw, ok := current["masterUrl"].(string); ok {
			*candidates = append(*candidates, xiaohongshuVideoCandidate{url: raw, score: score + 2})
		}
		if backups, ok := current["backupUrls"].([]any); ok {
			for _, raw := range backups {
				if value, ok := raw.(string); ok {
					*candidates = append(*candidates, xiaohongshuVideoCandidate{url: value, score: score + 1})
				}
			}
		}
		for key, child := range current {
			if key != "masterUrl" && key != "backupUrls" {
				collectXiaohongshuStreams(child, candidates)
			}
		}
	}
}

func xiaohongshuImageURLs(note map[string]any) [][]string {
	images, _ := note["imageList"].([]any)
	result := make([][]string, 0, len(images))
	for _, rawImage := range images {
		image, _ := rawImage.(map[string]any)
		candidates := make([]string, 0, 4)
		for _, key := range []string{"urlDefault", "urlPre", "url"} {
			if raw, ok := image[key].(string); ok {
				candidates = append(candidates, raw)
			}
		}
		if infoList, ok := image["infoList"].([]any); ok {
			for _, rawInfo := range infoList {
				info, _ := rawInfo.(map[string]any)
				if raw, ok := info["url"].(string); ok {
					candidates = append(candidates, raw)
				}
			}
		}
		seen := make(map[string]bool)
		trusted := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			if normalized := trustedXiaohongshuMediaURL(candidate); normalized != "" && !seen[normalized] {
				seen[normalized] = true
				trusted = append(trusted, normalized)
			}
		}
		if len(trusted) > 0 {
			result = append(result, trusted)
		}
	}
	return result
}

func trustedXiaohongshuMediaURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(strings.ReplaceAll(raw, `\u0026`, "&")))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		!hostMatches(parsed.Hostname(), "xhscdn.com", "xiaohongshu.com") {
		return ""
	}
	return parsed.String()
}

func xiaohongshuCaption(note map[string]any) string {
	title := cleanText(firstMapString(note, "title"))
	description := cleanText(firstMapString(note, "desc", "description"))
	parts := make([]string, 0, 3)
	if title != "" {
		parts = append(parts, title)
	}
	if description != "" && description != title {
		parts = append(parts, description)
	}
	if user, ok := note["user"].(map[string]any); ok {
		if author := cleanText(firstMapString(user, "nickname", "name")); author != "" {
			parts = append(parts, author)
		}
	}
	return strings.Join(parts, "\n\n")
}

func int64FromJSON(value any) int64 {
	switch current := value.(type) {
	case float64:
		return int64(current)
	case json.Number:
		result, _ := current.Int64()
		return result
	case int64:
		return current
	case int:
		return int64(current)
	default:
		return 0
	}
}
