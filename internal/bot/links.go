package bot

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/hyphentae/nyande-bot/internal/downloader"
	"github.com/hyphentae/nyande-bot/internal/telegram"
	"github.com/hyphentae/nyande-bot/resources"
)

var urlPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"']+`)

type allowlist struct {
	mu      sync.RWMutex
	path    string
	domains map[string]struct{}
}

func loadAllowlist(path string) (*allowlist, error) {
	result := &allowlist{path: path, domains: make(map[string]struct{})}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}
	var stored struct {
		Domains []string `json:"domains"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	for _, domain := range stored.Domains {
		if normalized := normalizeDomain(domain); normalized != "" {
			result.domains[normalized] = struct{}{}
		}
	}
	return result, nil
}

func (a *allowlist) Allows(raw string) bool {
	domain := normalizeDomain(raw)
	if domain == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for allowed := range a.domains {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func (a *allowlist) Add(raw string) (string, bool, error) {
	domain := normalizeDomain(raw)
	if domain == "" {
		return "", false, errors.New("invalid domain")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.domains[domain]; exists {
		return domain, false, nil
	}
	a.domains[domain] = struct{}{}
	values := make([]string, 0, len(a.domains))
	for value := range a.domains {
		values = append(values, value)
	}
	sort.Strings(values)
	data, _ := json.MarshalIndent(struct {
		Domains []string `json:"domains"`
	}{values}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return "", false, err
	}
	temporary := a.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return "", false, err
	}
	if err := os.Rename(temporary, a.path); err != nil {
		return "", false, err
	}
	return domain, true, nil
}

func normalizeDomain(raw string) string {
	raw = normalizeLink(raw)
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
}

func normalizeLink(raw string) string {
	raw = strings.TrimSpace(strings.TrimRight(raw, ".,;:!?)]}"))
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return raw
}

func extractURLs(message *telegram.Message) []string {
	text := message.ContentText()
	result := make([]string, 0)
	seen := map[string]bool{}
	add := func(raw string) {
		value := normalizeLink(raw)
		if value == "" || seen[value] {
			return
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" {
			return
		}
		seen[value] = true
		result = append(result, value)
	}
	for _, match := range urlPattern.FindAllString(text, -1) {
		add(match)
	}
	for _, entity := range message.ContentEntities() {
		switch entity.Type {
		case "text_link":
			add(entity.URL)
		case "url":
			add(sliceUTF16(text, entity.Offset, entity.Length))
		}
	}
	return result
}

func sliceUTF16(value string, offset, length int) string {
	encoded := utf16.Encode([]rune(value))
	if offset < 0 || length <= 0 || offset >= len(encoded) {
		return ""
	}
	end := offset + length
	if end > len(encoded) {
		end = len(encoded)
	}
	return string(utf16.Decode(encoded[offset:end]))
}

type adminEntry struct {
	admin   bool
	expires time.Time
}

func (b *Bot) isAdmin(ctx context.Context, message *telegram.Message) (bool, error) {
	if message.IsPrivate() {
		return true, nil
	}
	if message.From == nil {
		return false, nil
	}
	key := adminKey{ChatID: message.Chat.ID, UserID: message.From.ID}
	b.adminMu.Lock()
	cached, exists := b.adminCache[key]
	b.adminMu.Unlock()
	if exists && time.Now().Before(cached.expires) {
		return cached.admin, nil
	}
	member, err := b.telegram.GetChatMember(ctx, message.Chat.ID, message.From.ID)
	if err != nil {
		return false, err
	}
	admin := member.Status == "administrator" || member.Status == "creator"
	b.adminMu.Lock()
	b.adminCache[key] = adminEntry{admin: admin, expires: time.Now().Add(b.cfg.AdminCacheTTL)}
	b.adminMu.Unlock()
	return admin, nil
}

func (b *Bot) linkDeletionEnabled(chatID int64) bool {
	return b.linkConfig == nil || b.linkConfig.Enabled(chatID)
}

func (b *Bot) configureLinkDeletion(ctx context.Context, message *telegram.Message, argument string) error {
	if message.IsPrivate() {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.moderation.group_only"), message.MessageID, nil)
		return err
	}
	admin, err := b.isAdmin(ctx, message)
	if err != nil {
		return err
	}
	if !admin {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.moderation.admin_only"), message.MessageID, nil)
		return err
	}

	value := strings.ToLower(strings.TrimSpace(argument))
	if value == "" {
		key := "links.moderation.status_enabled"
		if !b.linkDeletionEnabled(message.Chat.ID) {
			key = "links.moderation.status_disabled"
		}
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
		return err
	}

	enabled := false
	switch value {
	case "on":
		enabled = true
	case "off":
	default:
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.moderation.usage"), message.MessageID, nil)
		return err
	}
	if b.linkConfig == nil {
		return errors.New("link deletion settings are unavailable")
	}
	if err := b.linkConfig.Set(message.Chat.ID, enabled); err != nil {
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.moderation.save_failed"), message.MessageID, nil)
		return errors.Join(err, sendErr)
	}
	key := "links.moderation.disabled"
	if enabled {
		key = "links.moderation.enabled"
	}
	_, err = b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
	return err
}

func supportedURLs(urls []string) []string {
	result := make([]string, 0, len(urls))
	for _, value := range urls {
		if downloader.Supported(value) {
			result = append(result, value)
		}
	}
	return result
}
