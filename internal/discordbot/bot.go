package discordbot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/logutil"
	"github.com/mmionya/nyande-bot/resources"
)

const (
	maxFilesPerMessage = 10
	downloadTimeout    = 5 * time.Minute
)

var discordURLPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"']+`)

type Bot struct {
	cfg        config.Config
	session    *discordgo.Session
	downloader *downloader.Downloader
	semaphore  chan struct{}
	startedAt  time.Time

	ctxMu sync.RWMutex
	ctx   context.Context

	statsMu        sync.Mutex
	uniqueChannels map[string]struct{}
	messages       atomic.Int64
	commands       atomic.Int64
	mediaTotal     atomic.Int64
	mediaErrors    atomic.Int64
}

func New(cfg config.Config) (*Bot, error) {
	if strings.TrimSpace(cfg.DiscordToken) == "" {
		return nil, errors.New("DISCORD_BOT_TOKEN is empty")
	}
	session, err := discordgo.New("Bot " + strings.TrimSpace(cfg.DiscordToken))
	if err != nil {
		return nil, fmt.Errorf("create Discord session: %w", err)
	}
	session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	result := &Bot{
		cfg: cfg, session: session, downloader: downloader.New(cfg),
		semaphore: make(chan struct{}, 8), startedAt: time.Now(),
		uniqueChannels: make(map[string]struct{}),
	}
	session.AddHandler(result.handleMessageCreate)
	return result, nil
}

func (b *Bot) Run(ctx context.Context) error {
	b.ctxMu.Lock()
	b.ctx = ctx
	b.ctxMu.Unlock()

	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open Discord gateway: %w", err)
	}
	identity := "Discord bot"
	if b.session.State != nil && b.session.State.User != nil {
		identity = b.session.State.User.Username
	}
	log.Printf("[discord] started as %s, command_prefix=%q", identity, b.cfg.DiscordPrefix)
	if err := b.session.UpdateGameStatus(0, resources.Format("discord_activity", map[string]any{"prefix": b.cfg.DiscordPrefix})); err != nil {
		log.Printf("[discord] could not update status: %v", err)
	}

	<-ctx.Done()
	return ctx.Err()
}

func (b *Bot) Close() error {
	return b.session.Close()
}

func (b *Bot) handleMessageCreate(session *discordgo.Session, event *discordgo.MessageCreate) {
	if event == nil || event.Message == nil || event.Author == nil || event.Author.Bot {
		return
	}
	if session.State != nil && session.State.User != nil && event.Author.ID == session.State.User.ID {
		return
	}

	ctx := b.currentContext()
	select {
	case b.semaphore <- struct{}{}:
		defer func() { <-b.semaphore }()
	case <-ctx.Done():
		return
	}

	b.messages.Add(1)
	b.trackChannel(event.ChannelID)
	if command, ok := parseCommand(event.Content, b.cfg.DiscordPrefix); ok {
		if b.handleCommand(session, event.Message, command) {
			return
		}
	}

	for _, candidate := range extractURLs(event.Content) {
		if downloader.Supported(candidate) {
			b.handleMedia(session, event.Message, candidate)
			return
		}
	}
}

func (b *Bot) handleCommand(session *discordgo.Session, message *discordgo.Message, command string) bool {
	started := time.Now()
	author := ""
	if message.Author != nil {
		author = message.Author.ID
	}
	log.Printf("[command] started platform=discord chat=%s user=%s msg=%s command=%q", message.ChannelID, author, message.ID, command)
	var response string
	switch command {
	case "gif":
		b.commands.Add(1)
		err := b.handleGIF(session, message)
		log.Printf("[command] finished platform=discord chat=%s user=%s msg=%s command=%q failed=%t elapsed_ms=%d", message.ChannelID, author, message.ID, command, err != nil, time.Since(started).Milliseconds())
		return true
	case "help":
		response = resources.Format("discord_help", map[string]any{"prefix": b.cfg.DiscordPrefix})
	case "ping":
		response = resources.Get("discord_ping")
	case "stats":
		response = b.statsText()
	default:
		log.Printf("[command] unknown platform=discord chat=%s msg=%s command=%q", message.ChannelID, message.ID, command)
		return false
	}
	b.commands.Add(1)
	_, err := session.ChannelMessageSendReply(message.ChannelID, response, message.SoftReference())
	if err != nil {
		log.Printf("[discord] command %s response failed: %v", command, err)
	}
	log.Printf("[command] finished platform=discord chat=%s user=%s msg=%s command=%q failed=%t elapsed_ms=%d", message.ChannelID, author, message.ID, command, err != nil, time.Since(started).Milliseconds())
	return true
}

func (b *Bot) handleMedia(session *discordgo.Session, message *discordgo.Message, mediaURL string) {
	started := time.Now()
	log.Printf("[media] download_started platform=discord chat=%s msg=%s url=%q", message.ChannelID, message.ID, logutil.URL(mediaURL))
	b.mediaTotal.Add(1)
	status, err := session.ChannelMessageSendReply(message.ChannelID, resources.Get("discord_downloading"), message.SoftReference())
	if err != nil {
		b.mediaErrors.Add(1)
		log.Printf("[discord] could not send download status: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(b.currentContext(), downloadTimeout)
	defer cancel()
	result, err := b.downloader.Download(ctx, mediaURL)
	if err != nil {
		b.mediaErrors.Add(1)
		log.Printf("[media] download_failed platform=discord chat=%s msg=%s url=%q elapsed_ms=%d: %v", message.ChannelID, message.ID, logutil.URL(mediaURL), time.Since(started).Milliseconds(), err)
		_, _ = session.ChannelMessageEdit(message.ChannelID, status.ID, downloadErrorText(err))
		return
	}
	if len(result.Items) == 0 {
		log.Printf("[media] download_empty platform=discord chat=%s msg=%s source=%q", message.ChannelID, message.ID, result.Source)
		b.mediaErrors.Add(1)
		_, _ = session.ChannelMessageEdit(message.ChannelID, status.ID, resources.Get("discord_media_not_found"))
		return
	}

	log.Printf("[media] downloaded platform=discord chat=%s msg=%s source=%q items=%d elapsed_ms=%d", message.ChannelID, message.ID, result.Source, len(result.Items), time.Since(started).Milliseconds())
	for index, item := range result.Items {
		log.Printf("[media] file platform=discord chat=%s msg=%s index=%d kind=%q name=%q bytes=%d", message.ChannelID, message.ID, index+1, item.Kind, item.Name, len(item.Data))
	}
	sendStarted := time.Now()
	log.Printf("[media] send_started platform=discord chat=%s msg=%s items=%d", message.ChannelID, message.ID, len(result.Items))
	_, _ = session.ChannelMessageEdit(message.ChannelID, status.ID, resources.Get("discord_sending"))
	if err := sendResult(session, message, result); err != nil {
		b.mediaErrors.Add(1)
		log.Printf("[media] send_failed platform=discord chat=%s msg=%s elapsed_ms=%d: %v", message.ChannelID, message.ID, time.Since(sendStarted).Milliseconds(), err)
		_, _ = session.ChannelMessageEdit(message.ChannelID, status.ID, resources.Get("discord_send_failed"))
		return
	}
	log.Printf("[media] sent platform=discord chat=%s msg=%s items=%d send_ms=%d total_ms=%d", message.ChannelID, message.ID, len(result.Items), time.Since(sendStarted).Milliseconds(), time.Since(started).Milliseconds())
	if err := session.ChannelMessageDelete(message.ChannelID, status.ID); err != nil {
		log.Printf("[discord] could not remove download status: %v", err)
	}
}

func sendResult(session *discordgo.Session, message *discordgo.Message, result downloader.Result) error {
	for start := 0; start < len(result.Items); start += maxFilesPerMessage {
		end := min(start+maxFilesPerMessage, len(result.Items))
		files := make([]*discordgo.File, 0, end-start)
		for index, item := range result.Items[start:end] {
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = resources.Format("discord_media_filename", map[string]any{"index": fmt.Sprintf("%02d", start+index+1)})
			}
			files = append(files, &discordgo.File{
				Name: name, ContentType: item.MIME, Reader: bytes.NewReader(item.Data),
			})
		}
		payload := &discordgo.MessageSend{
			Files: files,
			AllowedMentions: &discordgo.MessageAllowedMentions{
				Parse: []discordgo.AllowedMentionType{},
			},
		}
		if start == 0 {
			payload.Content = truncateMessage(result.Caption, 2000)
			payload.Reference = message.SoftReference()
		}
		if _, err := session.ChannelMessageSendComplex(message.ChannelID, payload); err != nil {
			return err
		}
	}
	return nil
}

func extractURLs(text string) []string {
	result := make([]string, 0)
	seen := make(map[string]struct{})
	for _, raw := range discordURLPattern.FindAllString(text, -1) {
		value := normalizeLink(raw)
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func normalizeLink(raw string) string {
	raw = strings.TrimSpace(strings.TrimRight(raw, ".,;:!?)]}"))
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return raw
}

func parseCommand(text, prefix string) (string, bool) {
	prefix = strings.TrimSpace(prefix)
	fields := strings.Fields(text)
	if prefix == "" || len(fields) == 0 || !strings.HasPrefix(fields[0], prefix) {
		return "", false
	}
	command := strings.TrimPrefix(fields[0], prefix)
	if command == "" {
		return "", false
	}
	return strings.ToLower(command), true
}

func (b *Bot) currentContext() context.Context {
	b.ctxMu.RLock()
	defer b.ctxMu.RUnlock()
	if b.ctx == nil {
		return context.Background()
	}
	return b.ctx
}

func (b *Bot) trackChannel(channelID string) {
	b.statsMu.Lock()
	b.uniqueChannels[channelID] = struct{}{}
	b.statsMu.Unlock()
}

func (b *Bot) statsText() string {
	b.statsMu.Lock()
	channels := len(b.uniqueChannels)
	b.statsMu.Unlock()
	total := b.mediaTotal.Load()
	errorsCount := b.mediaErrors.Load()
	errorRate := float64(0)
	if total > 0 {
		errorRate = float64(errorsCount) / float64(total) * 100
	}
	return resources.Format("discord_stats", map[string]any{
		"uptime":        time.Since(b.startedAt).Round(time.Second),
		"channels":      channels,
		"messages":      b.messages.Load(),
		"commands":      b.commands.Load(),
		"media_total":   total,
		"media_success": total - errorsCount,
		"media_errors":  errorsCount,
		"error_rate":    fmt.Sprintf("%.1f", errorRate),
	})
}

func downloadErrorText(err error) string {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "too large") || strings.Contains(text, "exceeds"):
		return resources.Get("discord_download_too_large")
	case strings.Contains(text, "404") || strings.Contains(text, "not found"):
		return resources.Get("discord_download_not_found")
	case strings.Contains(text, "timeout") || strings.Contains(text, "deadline"):
		return resources.Get("discord_download_timeout")
	default:
		return resources.Get("discord_download_failed")
	}
}

func truncateMessage(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit < 1 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}
