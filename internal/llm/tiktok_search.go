package llm

import (
	"context"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

var tikTokVideoPath = regexp.MustCompile(`^/@[A-Za-z0-9_.]+/video/([0-9]+)$`)

// MediaSearchTools returns tools with a cache scoped to one user request.
func (c *Client) MediaSearchTools() []Tool {
	if !c.webSearchAvailable() {
		return nil
	}
	var mu sync.Mutex
	cache := map[string]string{}
	return []Tool{{
		Name:        "search_tiktok",
		Description: resources.Get("llm.tool.tiktok.description"),
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]string{"type": "string"}}, "required": []string{"query"}, "additionalProperties": false},
		Execute: func(ctx context.Context, args map[string]string) (string, error) {
			query := strings.Join(strings.Fields(args["query"]), " ")
			if query == "" {
				return resources.Get("llm.tool.tiktok.query_required"), nil
			}
			if len([]rune(query)) > 300 {
				return resources.Get("llm.tool.tiktok.query_too_long"), nil
			}
			mu.Lock()
			defer mu.Unlock()
			key := strings.ToLower(query)
			if result, ok := cache[key]; ok {
				return result, nil
			}
			result := c.searchTikTok(ctx, query)
			if err := ctx.Err(); err != nil {
				return "", err
			}
			cache[key] = result
			return result, nil
		},
	}}
}

func filterTikTokVideos(results []searchResult) []searchResult {
	var out []searchResult
	seen := map[string]bool{}
	for _, result := range results {
		u, err := url.Parse(result.URL)
		if err != nil || u.User != nil || u.Port() != "" || (u.Scheme != "https" && u.Scheme != "http") {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host != "www.tiktok.com" && host != "tiktok.com" && host != "m.tiktok.com" {
			continue
		}
		path := strings.TrimSuffix(u.Path, "/")
		match := tikTokVideoPath.FindStringSubmatch(path)
		if match == nil || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		result.URL = "https://www.tiktok.com" + path
		result.Snippet = truncate(result.Snippet, 500)
		out = append(out, result)
	}
	return out
}

func (c *Client) searchTikTok(ctx context.Context, query string) string {
	started := time.Now()
	searchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	maximum := c.cfg.LLMWebSearchResults
	if maximum < 1 {
		maximum = 5
	}
	if maximum > 10 {
		maximum = 10
	}
	var results []searchResult
	backend := "local"
	if c.usesOpenRouter() && c.serverSearchReady() {
		tool := openRouterWebSearchTool(maximum * 2)
		tool["parameters"].(map[string]any)["allowed_domains"] = []string{"tiktok.com"}
		serverCtx, serverCancel := context.WithTimeout(searchCtx, 30*time.Second)
		response, err := c.complete(serverCtx, []chatMessage{
			{Role: "system", Content: resources.Get("llm.prompt.tiktok_search")},
			{Role: "user", Content: query},
		}, []map[string]any{tool}, true)
		serverCancel()
		if err == nil && len(response.Choices) > 0 {
			// Only actual search citations are eligible; model-written URLs are not evidence.
			for _, a := range response.Choices[0].Message.Annotations {
				if a.Type == "url_citation" {
					results = append(results, searchResult{Title: a.Citation.Title, URL: a.Citation.URL, Snippet: a.Citation.Content})
				}
			}
			results = filterTikTokVideos(results)
		}
		log.Printf(resources.Get("llm.log.tiktok_server_results"), len(results), err != nil)
		if len(results) > 0 {
			backend = "server"
		}
	}
	if len(results) == 0 && searchCtx.Err() == nil {
		results = c.searchWebResults(searchCtx, query+" site:tiktok.com inurl:video", 30, filterTikTokVideos)
	}
	if len(results) > maximum {
		results = results[:maximum]
	}
	log.Printf(resources.Get("llm.log.tiktok_completed"), backend, len(results), time.Since(started).Milliseconds())
	if len(results) == 0 {
		return resources.Get("llm.search.tiktok_no_results")
	}
	return resources.Get("llm.search.tiktok_results_intro") + formatSearchResults(results)
}
