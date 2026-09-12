package llm

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	searchAnchor  = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	searchHref    = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)
	searchClass   = regexp.MustCompile(`(?is)\bclass\s*=\s*["'][^"']*\b(?:result__a|result-link)\b[^"']*["']`)
	searchSnippet = regexp.MustCompile(`(?is)<(?:a|div|span)\b[^>]*class\s*=\s*["'][^"']*\b(?:result__snippet|result-snippet)\b[^"']*["'][^>]*>(.*?)</(?:a|div|span)>`)
	htmlTag       = regexp.MustCompile(`(?is)<[^>]+>`)
)

func (c *Client) searchWeb(ctx context.Context, query string) string {
	if strings.TrimSpace(query) == "" {
		return resources.Get("llm.search.no_results")
	}
	results := c.searchWebResults(ctx, query, c.cfg.LLMWebSearchResults, nil)
	if len(results) == 0 {
		return resources.Get("llm.search.unavailable")
	}
	return formatSearchResults(results)
}

func formatSearchResults(results []searchResult) string {
	parts := []string{resources.Get("llm.search.results_intro")}
	for index, result := range results {
		entry := resources.Format("llm.search.result", map[string]any{
			"index": index + 1, "title": result.Title, "url": result.URL,
		})
		if result.Snippet != "" {
			entry += resources.Get("llm.search.snippet") + result.Snippet
		}
		parts = append(parts, entry)
	}
	return strings.Join(parts, "\n\n")
}

func (c *Client) searchWebResults(ctx context.Context, query string, maximum int, filter func([]searchResult) []searchResult) []searchResult {
	started := time.Now()
	if maximum < 1 {
		maximum = 5
	}
	if strings.TrimSpace(query) == "" {
		return nil
	}
	for _, endpoint := range []string{"https://html.duckduckgo.com/html/", "https://lite.duckduckgo.com/lite/"} {
		log.Printf("[search] started backend=%q", endpoint)
		form := url.Values{"q": {query}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			continue
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/131 Safari/537.36")
		request.Header.Set("Accept-Language", "ru,en;q=0.8")
		response, err := c.http.Do(request)
		if err != nil {
			log.Printf("[search] request_failed backend=%q elapsed_ms=%d", endpoint, time.Since(started).Milliseconds())
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 3*1024*1024))
		response.Body.Close()
		if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			log.Printf("[search] response_failed backend=%q status=%d read_error=%t", endpoint, response.StatusCode, readErr != nil)
			continue
		}
		results := parseSearchResults(string(body), maximum)
		if filter != nil {
			results = filter(results)
		}
		if len(results) == 0 {
			log.Printf("[search] no_results backend=%q status=%d", endpoint, response.StatusCode)
			continue
		}
		log.Printf("[search] completed backend=%q results=%d elapsed_ms=%d", endpoint, len(results), time.Since(started).Milliseconds())
		return results
	}
	log.Printf("[search] unavailable elapsed_ms=%d", time.Since(started).Milliseconds())
	return nil
}

func parseSearchResults(page string, maximum int) []searchResult {
	snippets := make([]string, 0)
	for _, match := range searchSnippet.FindAllStringSubmatch(page, -1) {
		if len(match) == 2 {
			snippets = append(snippets, cleanHTML(match[1]))
		}
	}
	seen := map[string]bool{}
	results := make([]searchResult, 0, maximum)
	for _, match := range searchAnchor.FindAllStringSubmatch(page, -1) {
		if len(results) >= maximum || len(match) != 3 {
			break
		}
		href := searchHref.FindStringSubmatch(match[1])
		if len(href) != 2 || (!searchClass.MatchString(match[1]) && !strings.Contains(href[1], "uddg=")) {
			continue
		}
		resultURL := decodeDuckDuckGoURL(href[1])
		title := cleanHTML(match[2])
		if resultURL == "" || title == "" || seen[resultURL] {
			continue
		}
		seen[resultURL] = true
		snippet := ""
		if len(snippets) > len(results) {
			snippet = snippets[len(results)]
		}
		results = append(results, searchResult{Title: title, URL: resultURL, Snippet: snippet})
	}
	return results
}

func decodeDuckDuckGoURL(raw string) string {
	raw = html.UnescapeString(raw)
	switch {
	case strings.HasPrefix(raw, "//"):
		raw = "https:" + raw
	case strings.HasPrefix(raw, "/"):
		raw = "https://duckduckgo.com" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.Hostname() == "duckduckgo.com" && parsed.Path == "/l/" {
		return parsed.Query().Get("uddg")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return ""
	}
	return parsed.String()
}

func cleanHTML(value string) string {
	return strings.Join(strings.Fields(html.UnescapeString(htmlTag.ReplaceAllString(value, ""))), " ")
}

func formatSearchError(err error) string {
	return fmt.Sprintf("%s: %v", resources.Get("llm.search.unavailable"), err)
}
