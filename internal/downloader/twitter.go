package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mmionya/nyande-bot/resources"
)

var twitterPathPattern = regexp.MustCompile(`(?i)^/([a-z0-9_]+|i/web)/status/([1-9][0-9]*)(?:/(?:photo|video)/[1-9][0-9]*)?/?$`)

type twitterTweet struct {
	ID     string          `json:"id"`
	Text   string          `json:"text"`
	Media  json.RawMessage `json:"media"`
	Author struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
	} `json:"author"`
}

type twitterMedia struct {
	URL  string `json:"url"`
	Type string `json:"type"`
}

func twitterPostURL(value *url.URL) (canonical, id string, err error) {
	if !IsTwitter(value) || value.User != nil || value.Port() != "" {
		return "", "", errors.New(resources.Get("downloader.error.unsupported_twitter_x_post_url"))
	}
	match := twitterPathPattern.FindStringSubmatch(value.Path)
	if len(match) != 3 {
		return "", "", errors.New(resources.Get("downloader.error.unsupported_twitter_x_post_url"))
	}
	if _, err := strconv.ParseUint(match[2], 10, 64); err != nil {
		return "", "", errors.New(resources.Get("downloader.error.unsupported_twitter_x_post_url"))
	}
	user := match[1]
	if strings.EqualFold(user, "i/web") {
		user = "i"
	}
	return "https://x.com/" + user + "/status/" + match[2], match[2], nil
}

// TwitterPost fetches the post text and attribution without downloading media.
func (d *Downloader) TwitterPost(ctx context.Context, rawURL string) (Post, error) {
	value, err := normalizedURL(rawURL)
	if err != nil {
		return Post{}, err
	}
	canonical, id, err := twitterPostURL(value)
	if err != nil {
		return Post{}, err
	}
	attempts := max(1, d.cfg.RetryAttempts)
	for attempt := 1; attempt <= attempts; attempt++ {
		var post Post
		post, _, err = d.fetchTwitterPost(ctx, canonical, id)
		if err == nil {
			return post, nil
		}
		if ctx.Err() != nil {
			return Post{}, ctx.Err()
		}
		if !retryable(err) || attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return Post{}, ctx.Err()
		case <-time.After(d.cfg.RetryDelay * time.Duration(attempt)):
		}
	}
	return Post{}, err
}

func (d *Downloader) fetchTwitterPost(ctx context.Context, canonical, id string) (Post, json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return Post{}, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.fxtwitter.com/status/"+id, nil)
	if err != nil {
		return Post{}, nil, err
	}
	request.Header.Set("User-Agent", "nyande-bot/2.0")
	response, err := d.client.Do(request)
	if err != nil {
		return Post{}, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Post{}, nil, fmt.Errorf(resources.Get("downloader.error.fxtwitter_http"), response.StatusCode)
	}
	var payload struct {
		Code  int           `json:"code"`
		Tweet *twitterTweet `json:"tweet"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&payload); err != nil {
		return Post{}, nil, fmt.Errorf(resources.Get("downloader.error.fxtwitter_invalid_json"), err)
	}
	if payload.Code != http.StatusOK {
		return Post{}, nil, fmt.Errorf(resources.Get("downloader.error.fxtwitter_http"), payload.Code)
	}
	if payload.Tweet == nil || payload.Tweet.ID != id {
		return Post{}, nil, errors.New(resources.Get("twitter.error.invalid_json"))
	}
	tweet := payload.Tweet
	author := strings.TrimSpace(tweet.Author.Name)
	if tweet.Author.ScreenName != "" {
		author = strings.TrimSpace(author + " (@" + tweet.Author.ScreenName + ")")
	}
	post := Post{
		Text:   strings.TrimSpace(html.UnescapeString(strings.ReplaceAll(tweet.Text, "\r\n", "\n"))),
		Author: cleanText(author),
		URL:    canonical,
	}
	return post, tweet.Media, nil
}

func (d *Downloader) downloadTwitter(ctx context.Context, value *url.URL) (Result, error) {
	canonical, id, err := twitterPostURL(value)
	if err != nil {
		return Result{}, err
	}
	post, rawMedia, err := d.fetchTwitterPost(ctx, canonical, id)
	if err != nil {
		return Result{}, err
	}
	urls, declared, err := twitterMediaURLs(rawMedia)
	if err != nil {
		return Result{}, err
	}
	result := Result{Post: &post, Caption: cleanText(strings.TrimSpace(post.Text + "\n\n" + post.Author)), Source: "twitter"}
	if !declared {
		if post.Text == "" {
			return Result{}, errors.New(resources.Get("twitter.error.no_media"))
		}
		return result, nil
	}
	seen := map[string]bool{}
	for _, candidate := range urls {
		if seen[candidate.URL] {
			continue
		}
		seen[candidate.URL] = true
		parsed, err := url.Parse(candidate.URL)
		if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || !hostMatches(parsed.Hostname(), "twimg.com") {
			continue
		}
		item, err := d.fetchMedia(ctx, parsed.String(), candidate.Type)
		if err != nil {
			return Result{}, err
		}
		result.Items = append(result.Items, item)
		if len(result.Items) >= d.cfg.MaxMediaItems {
			break
		}
	}
	if len(result.Items) == 0 {
		return Result{}, errors.New(resources.Get("downloader.error.fxtwitter_returned_no_trusted_media"))
	}
	return result, nil
}

func twitterMediaURLs(raw json.RawMessage) ([]twitterMedia, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var media struct {
		Videos []twitterMedia `json:"videos"`
		Photos []twitterMedia `json:"photos"`
		All    []twitterMedia `json:"all"`
	}
	if err := json.Unmarshal(raw, &media); err != nil {
		return nil, false, fmt.Errorf(resources.Get("downloader.error.fxtwitter_invalid_json"), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, fmt.Errorf(resources.Get("downloader.error.fxtwitter_invalid_json"), err)
	}
	declared := len(media.Videos)+len(media.Photos)+len(media.All) > 0
	for key, value := range fields {
		if key != "videos" && key != "photos" && key != "all" && string(value) != "null" {
			declared = true
		}
	}
	urls := make([]twitterMedia, 0, len(media.Videos)+len(media.Photos))
	for _, video := range media.Videos {
		urls = append(urls, twitterMedia{URL: video.URL, Type: "video"})
	}
	for _, photo := range media.Photos {
		urls = append(urls, twitterMedia{URL: photo.URL, Type: "photo"})
	}
	if len(urls) == 0 {
		for _, item := range media.All {
			switch item.Type {
			case "photo":
				urls = append(urls, item)
			case "video", "gif":
				urls = append(urls, twitterMedia{URL: item.URL, Type: "video"})
			}
		}
	}
	return urls, declared, nil
}
