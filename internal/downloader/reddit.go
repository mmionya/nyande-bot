package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func (d *Downloader) downloadReddit(ctx context.Context, value *url.URL) (Result, error) {
	resolved, err := d.resolveRedditURL(ctx, value)
	if err != nil {
		return d.downloadYTDLP(ctx, value)
	}
	jsonURL := *resolved
	jsonURL.RawQuery = ""
	jsonURL.Fragment = ""
	jsonURL.Path = strings.TrimRight(jsonURL.Path, "/") + ".json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, jsonURL.String(), nil)
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("User-Agent", d.cfg.RedditUserAgent)
	response, err := d.client.Do(request)
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			var payload any
			if json.NewDecoder(response.Body).Decode(&payload) == nil {
				post := redditPostData(payload)
				if post != nil {
					caption := firstMapString(post, "title", "selftext")
					imageURLs := redditImageURLs(post)
					if len(imageURLs) > 0 {
						items := make([]Media, 0, len(imageURLs))
						for _, imageURL := range imageURLs {
							item, downloadErr := d.fetchMedia(ctx, imageURL, "photo")
							if downloadErr != nil {
								continue
							}
							items = append(items, item)
						}
						if len(items) > 0 {
							return Result{Items: items, Caption: cleanText(caption), Source: "reddit"}, nil
						}
					}
				}
			}
		}
	}
	return d.downloadYTDLP(ctx, resolved)
}

func (d *Downloader) resolveRedditURL(ctx context.Context, value *url.URL) (*url.URL, error) {
	if !hostMatches(value.Hostname(), "redd.it") {
		return value, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, value.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", d.cfg.RedditUserAgent)
	response, err := d.client.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	if !hostMatches(response.Request.URL.Hostname(), "reddit.com", "redd.it") {
		return nil, errors.New("Reddit short link redirected outside Reddit")
	}
	return response.Request.URL, nil
}

func redditPostData(payload any) map[string]any {
	listings, ok := payload.([]any)
	if !ok || len(listings) == 0 {
		return nil
	}
	listing, _ := listings[0].(map[string]any)
	data, _ := listing["data"].(map[string]any)
	children, _ := data["children"].([]any)
	if len(children) == 0 {
		return nil
	}
	child, _ := children[0].(map[string]any)
	post, _ := child["data"].(map[string]any)
	return post
}

func redditImageURLs(post map[string]any) []string {
	result := make([]struct {
		order int
		url   string
	}, 0)
	if gallery, ok := post["gallery_data"].(map[string]any); ok {
		items, _ := gallery["items"].([]any)
		metadata, _ := post["media_metadata"].(map[string]any)
		for index, raw := range items {
			item, _ := raw.(map[string]any)
			id, _ := item["media_id"].(string)
			meta, _ := metadata[id].(map[string]any)
			source, _ := meta["s"].(map[string]any)
			link := firstMapString(source, "u", "gif", "mp4")
			if link != "" && kindFromExtension(strings.Split(link, "?")[0]) == "photo" {
				result = append(result, struct {
					order int
					url   string
				}{index, html.UnescapeString(link)})
			}
		}
	}
	if len(result) == 0 {
		if preview, ok := post["preview"].(map[string]any); ok {
			images, _ := preview["images"].([]any)
			for index, raw := range images {
				image, _ := raw.(map[string]any)
				source, _ := image["source"].(map[string]any)
				link, _ := source["url"].(string)
				if link != "" {
					result = append(result, struct {
						order int
						url   string
					}{index, html.UnescapeString(link)})
				}
			}
		}
	}
	if len(result) == 0 {
		link := firstMapString(post, "url_overridden_by_dest", "url")
		if parsed, err := url.Parse(link); err == nil && redditImageHost(parsed.Hostname()) {
			result = append(result, struct {
				order int
				url   string
			}{0, link})
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].order < result[j].order })
	seen := map[string]bool{}
	urls := make([]string, 0, len(result))
	for _, candidate := range result {
		parsed, err := url.Parse(candidate.url)
		if err != nil || parsed.Scheme != "https" || !redditImageHost(parsed.Hostname()) || seen[candidate.url] {
			continue
		}
		seen[candidate.url] = true
		urls = append(urls, candidate.url)
	}
	return urls
}

func redditImageHost(host string) bool {
	return hostMatches(host, "redd.it", "redditmedia.com", "reddit.com")
}

func redditError(status int) error {
	return fmt.Errorf("Reddit HTTP %d", status)
}
