package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type tikWMResponse struct {
	Code int       `json:"code"`
	Msg  string    `json:"msg"`
	Data tikWMData `json:"data"`
}

type tikWMData struct {
	Title  string   `json:"title"`
	Play   string   `json:"play"`
	HDPlay string   `json:"hdplay"`
	WMPlay string   `json:"wmplay"`
	Images []string `json:"images"`
	Author struct {
		Nickname string `json:"nickname"`
		UniqueID string `json:"unique_id"`
	} `json:"author"`
}

func (d *Downloader) downloadTikTok(ctx context.Context, value *url.URL) (Result, error) {
	if err := d.waitTikTok(ctx); err != nil {
		return Result{}, err
	}
	form := url.Values{"url": {value.String()}, "hd": {"1"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.tikwm.com/api/", strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", browserUserAgent)
	request.Header.Set("Origin", "https://www.tikwm.com")
	response, err := d.client.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, fmt.Errorf("TikWM HTTP %d", response.StatusCode)
	}
	var payload tikWMResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return Result{}, fmt.Errorf("TikWM invalid response: %w", err)
	}
	if payload.Code != 0 {
		return Result{}, fmt.Errorf("TikWM could not process post: %s", payload.Msg)
	}

	mediaURLs := payload.Data.Images
	expected := "photo"
	if len(mediaURLs) == 0 {
		expected = "video"
		video := firstNonEmpty(payload.Data.HDPlay, payload.Data.Play, payload.Data.WMPlay)
		if video != "" {
			mediaURLs = []string{video}
		}
	}
	if len(mediaURLs) == 0 {
		return Result{}, errors.New("TikWM returned no downloadable media")
	}
	items := make([]Media, 0, len(mediaURLs))
	for _, raw := range mediaURLs {
		if strings.HasPrefix(raw, "/") {
			raw = "https://www.tikwm.com" + raw
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" {
			continue
		}
		item, err := d.fetchMedia(ctx, parsed.String(), expected)
		if err != nil {
			return Result{}, err
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return Result{}, errors.New("TikWM returned no valid media URLs")
	}
	caption := cleanText(payload.Data.Title)
	if payload.Data.Author.Nickname != "" {
		author := payload.Data.Author.Nickname
		if payload.Data.Author.UniqueID != "" {
			author += " (@" + payload.Data.Author.UniqueID + ")"
		}
		if caption != "" {
			caption += "\n\n"
		}
		caption += author
	}
	return Result{Items: items, Caption: caption, Source: "tiktok"}, nil
}

func (d *Downloader) waitTikTok(ctx context.Context) error {
	d.apiMu.Lock()
	defer d.apiMu.Unlock()
	delay := 1100*time.Millisecond - time.Since(d.lastTikTok)
	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	d.lastTikTok = time.Now()
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
