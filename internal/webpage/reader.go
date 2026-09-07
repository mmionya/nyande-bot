package webpage

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Page struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

var (
	titlePattern   = regexp.MustCompile(`(?i)<title[^>]*>([\s\S]*?)</title>`)
	stripPatterns  = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script[^>]*>[\s\S]*?</script>`),
		regexp.MustCompile(`(?is)<style[^>]*>[\s\S]*?</style>`),
		regexp.MustCompile(`(?is)<noscript[^>]*>[\s\S]*?</noscript>`),
		regexp.MustCompile(`(?is)<svg[^>]*>[\s\S]*?</svg>`),
		regexp.MustCompile(`(?is)<nav[^>]*>[\s\S]*?</nav>`),
		regexp.MustCompile(`(?is)<header[^>]*>[\s\S]*?</header>`),
		regexp.MustCompile(`(?is)<footer[^>]*>[\s\S]*?</footer>`),
		regexp.MustCompile(`(?is)<!--[\s\S]*?-->`),
	}
	tagPattern     = regexp.MustCompile(`(?s)<[^>]+>`)
	newlinePattern = regexp.MustCompile(`\n{3,}`)
	spacePattern   = regexp.MustCompile(`[ \t]{2,}`)
)

var defaultClient = &http.Client{
	Timeout: 15 * time.Second,
}

func Fetch(ctx context.Context, rawURL string, maxRunes int) (Page, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return Page{}, errors.New("empty URL")
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return Page{}, fmt.Errorf("invalid URL: %s", rawURL)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Page{}, err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; nyande-bot/2.0; +https://github.com/mmionya/nyande-bot)")
	request.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.8")

	response, err := defaultClient.Do(request)
	if err != nil {
		return Page{}, fmt.Errorf("fetch %s failed: %w", rawURL, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Page{}, fmt.Errorf("HTTP error %d (%s)", response.StatusCode, response.Status)
	}

	// Limit read size to 2 MB
	bodyBytes, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return Page{}, fmt.Errorf("read response body: %w", err)
	}

	rawHTML := string(bodyBytes)
	title := ""
	if match := titlePattern.FindStringSubmatch(rawHTML); len(match) > 1 {
		title = strings.TrimSpace(html.UnescapeString(match[1]))
	}

	cleanText := ExtractText(rawHTML)
	if maxRunes <= 0 {
		maxRunes = 6000
	}
	if utf8.RuneCountInString(cleanText) > maxRunes {
		runes := []rune(cleanText)
		cleanText = string(runes[:maxRunes]) + "…"
	}

	if cleanText == "" {
		cleanText = "Страница не содержит текстового контента."
	}

	return Page{
		URL:     rawURL,
		Title:   title,
		Content: cleanText,
	}, nil
}

func ExtractText(rawHTML string) string {
	for _, pattern := range stripPatterns {
		rawHTML = pattern.ReplaceAllString(rawHTML, " ")
	}
	// Replace block elements with newlines
	blockPattern := regexp.MustCompile(`(?i)</?(?:p|div|br|hr|h[1-6]|li|tr|table|blockquote)[^>]*>`)
	rawHTML = blockPattern.ReplaceAllString(rawHTML, "\n")
	text := tagPattern.ReplaceAllString(rawHTML, " ")
	text = html.UnescapeString(text)

	var lines []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(spacePattern.ReplaceAllString(line, " "))
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	joined := strings.Join(lines, "\n")
	return newlinePattern.ReplaceAllString(joined, "\n\n")
}
