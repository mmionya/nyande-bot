package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

var urlPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"']+`)

type allowlist struct {
	mu          sync.RWMutex
	path        string
	legacy      []string
	chatDomains map[int64]map[string]struct{}
}

type storedAllowlist struct {
	// Legacy global entries have unknown authorship, so preserve but never apply them.
	Domains     []string           `json:"domains,omitempty"`
	ChatDomains map[int64][]string `json:"chat_domains,omitempty"`
}

func loadAllowlist(path string) (*allowlist, error) {
	result := &allowlist{path: path, chatDomains: make(map[int64]map[string]struct{})}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}
	var stored storedAllowlist
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, err
	}
	result.legacy = stored.Domains
	if len(stored.Domains) > 0 {
		log.Printf("[links] %d legacy global allowlist entries are inactive; use /allowlink in each intended chat to authorize them again", len(stored.Domains))
	}
	for chatID, domains := range stored.ChatDomains {
		result.chatDomains[chatID] = make(map[string]struct{})
		for _, domain := range domains {
			if normalized := normalizeDomain(domain); normalized != "" {
				result.chatDomains[chatID][normalized] = struct{}{}
			}
		}
	}
	return result, nil
}

func (a *allowlist) Allows(chatID int64, raw string) bool {
	if a == nil {
		return false
	}
	domain := normalizeDomain(raw)
	if domain == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for allowed := range a.chatDomains[chatID] {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func (a *allowlist) Add(chatID int64, raw string) (string, bool, error) {
	domain := normalizeDomain(raw)
	if domain == "" {
		return "", false, errors.New(resources.Get("telegram.error.invalid_domain"))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.chatDomains[chatID][domain]; exists {
		return domain, false, nil
	}
	stored := storedAllowlist{Domains: a.legacy, ChatDomains: make(map[int64][]string, len(a.chatDomains)+1)}
	for id, domains := range a.chatDomains {
		for value := range domains {
			stored.ChatDomains[id] = append(stored.ChatDomains[id], value)
		}
	}
	stored.ChatDomains[chatID] = append(stored.ChatDomains[chatID], domain)
	for _, domains := range stored.ChatDomains {
		sort.Strings(domains)
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return "", false, err
	}
	// Persist before publishing, so failed writes cannot grant permission in memory.
	if err := writeLLMChatSettings(a.path, append(data, '\n')); err != nil {
		return "", false, err
	}
	if a.chatDomains == nil {
		a.chatDomains = make(map[int64]map[string]struct{})
	}
	if a.chatDomains[chatID] == nil {
		a.chatDomains[chatID] = make(map[string]struct{})
	}
	a.chatDomains[chatID][domain] = struct{}{}
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
	if message.SenderChat != nil {
		// Telegram represents anonymous group administrators as the group itself.
		// Messages sent on behalf of other channels are not admin messages.
		return (message.Chat.Type == "group" || message.Chat.Type == "supergroup") && message.SenderChat.ID == message.Chat.ID, nil
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
	return b.linkConfig != nil && b.linkConfig.Enabled(chatID)
}

// Report deletion so command dispatch cannot continue after its message was removed.
func (b *Bot) moderateCommandLinks(ctx context.Context, message *telegram.Message) (bool, error) {
	return b.moderateLinks(ctx, message, extractURLs(message))
}

func (b *Bot) moderateLinks(ctx context.Context, message *telegram.Message, urls []string) (bool, error) {
	if message.IsPrivate() || !b.linkDeletionEnabled(message.Chat.ID) {
		return false, nil
	}
	if b.silentModeration(message.Chat.ID) {
		if message.IsAutomaticForward || (message.SenderChat == nil && message.From != nil && b.linkConfig.UserPermitted(message.Chat.ID, message.From.ID)) {
			return false, nil
		}
	}
	forbidden := false
	for _, value := range urls {
		if b.silentModeration(message.Chat.ID) || (!downloader.AllowedHost(value) && !b.allowlist.Allows(message.Chat.ID, value)) {
			forbidden = true
			break
		}
	}
	if !forbidden {
		return false, nil
	}
	admin, err := b.isAdmin(ctx, message)
	if err != nil {
		return false, fmt.Errorf(resources.Get("telegram.error.link_author_role"), err)
	}
	if admin {
		return false, nil
	}
	if err := b.telegram.DeleteMessage(ctx, message.Chat.ID, message.MessageID); err != nil {
		return true, err
	}
	if !b.silentModeration(message.Chat.ID) {
		_, _ = b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.unsupported"), 0, nil)
	}
	return true, nil
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
		return errors.New(resources.Get("telegram.error.link_settings_unavailable"))
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
