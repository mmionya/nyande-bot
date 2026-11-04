package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/music"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

const musicPageSize = 5

type telegramMusic struct {
	mu       sync.Mutex
	sessions map[string]telegramMusicSearch
	busy     bool
	search   func(context.Context, string, int) ([]music.Track, error)
	download func(context.Context, music.Track) (downloader.Media, error)
}

type telegramMusicSearch struct {
	id                 string
	chatID, userID     int64
	messageID, replyTo int
	query, source      string
	tracks             []music.Track
	expires            time.Time
}

func (b *Bot) telegramMusic() *telegramMusic {
	b.musicMu.Lock()
	defer b.musicMu.Unlock()
	if b.music == nil {
		b.music = &telegramMusic{sessions: make(map[string]telegramMusicSearch), search: b.downloader.SearchMusic, download: b.downloader.DownloadMusic}
	}
	return b.music
}

func (m *telegramMusic) begin() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy {
		return false
	}
	m.busy = true
	return true
}
func (m *telegramMusic) finish() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }
func (m *telegramMusic) prune() {
	for id, s := range m.sessions {
		if time.Now().After(s.expires) {
			delete(m.sessions, id)
		}
	}
}
func (m *telegramMusic) put(s telegramMusicSearch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	if len(m.sessions) >= 128 {
		var oldest telegramMusicSearch
		for _, v := range m.sessions {
			if oldest.id == "" || v.expires.Before(oldest.expires) {
				oldest = v
			}
		}
		delete(m.sessions, oldest.id)
	}
	m.sessions[s.id] = s
}
func (m *telegramMusic) get(id string, chatID, userID int64, messageID int) (telegramMusicSearch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	s, ok := m.sessions[id]
	return s, ok && s.chatID == chatID && s.userID == userID && s.messageID == messageID
}
func (m *telegramMusic) latest(chatID, userID int64) (telegramMusicSearch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	var latest telegramMusicSearch
	for _, s := range m.sessions {
		if s.chatID == chatID && s.userID == userID && s.expires.After(latest.expires) {
			latest = s
		}
	}
	return latest, latest.id != ""
}
func (m *telegramMusic) remove(id string) { m.mu.Lock(); delete(m.sessions, id); m.mu.Unlock() }

func musicQuery(query string) (text, source string, err error) {
	id, err := music.Identifier(query)
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(id, "https://") {
		return id, "", nil
	}
	prefix, text, _ := strings.Cut(id, ":")
	return text, map[string]string{"ytsearch": "yt", "ytmsearch": "ytm", "scsearch": "sc", "bcsearch": "bc"}[prefix], nil
}

func (b *Bot) musicCommand(ctx context.Context, message *telegram.Message, command, query string) error {
	reply := func(key string) error {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
		return err
	}
	if userID(message) == 0 || message.SenderChat != nil {
		return reply("telegram.music.account_required")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return reply("telegram.music.help")
	}
	m := b.telegramMusic()
	if !m.begin() {
		return reply("telegram.music.busy")
	}
	defer m.finish()
	if command == "play" {
		if number, err := strconv.Atoi(query); err == nil {
			s, ok := m.latest(message.Chat.ID, userID(message))
			if !ok || number < 1 || number > len(s.tracks) {
				return reply("telegram.music.expired")
			}
			return b.sendMusic(ctx, message, s.tracks[number-1])
		}
	}
	text, source, err := musicQuery(query)
	if err != nil {
		return reply("telegram.music.help")
	}
	status, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("telegram.music.searching"), message.MessageID, nil)
	if err != nil {
		return err
	}
	limit := 20
	if command == "play" {
		limit = 1
	}
	tracks, err := m.search(ctx, query, limit)
	if err != nil {
		log.Printf("[telegram music] search failed: %v", err)
		return b.telegram.EditMessageText(ctx, message.Chat.ID, status.MessageID, resources.Get("telegram.music.search_failed"), nil)
	}
	if command == "play" {
		if len(tracks) == 0 {
			return b.telegram.EditMessageText(ctx, message.Chat.ID, status.MessageID, resources.Get("music.empty"), nil)
		}
		_ = b.telegram.DeleteMessage(ctx, message.Chat.ID, status.MessageID)
		return b.sendMusic(ctx, message, tracks[0])
	}
	s, err := newTelegramMusicSearch(message.Chat.ID, userID(message), status.MessageID, message.MessageID, text, source, tracks)
	if err != nil {
		return err
	}
	content, keyboard := s.page(0)
	m.put(s)
	if err := b.telegram.EditMessageText(ctx, s.chatID, s.messageID, content, keyboard); err != nil {
		m.remove(s.id)
		return err
	}
	return nil
}

func newTelegramMusicSearch(chatID, userID int64, messageID, replyTo int, query, source string, tracks []music.Track) (telegramMusicSearch, error) {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return telegramMusicSearch{}, err
	}
	return telegramMusicSearch{id: hex.EncodeToString(token[:]), chatID: chatID, userID: userID, messageID: messageID, replyTo: replyTo,
		query: query, source: source, tracks: tracks[:min(len(tracks), 20)], expires: time.Now().Add(10 * time.Minute)}, nil
}

func musicLabel(text string, length int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > length {
		return string(runes[:length-1]) + "…"
	}
	return string(runes)
}

func (s telegramMusicSearch) page(page int) (string, any) {
	pages := max(1, (len(s.tracks)+musicPageSize-1)/musicPageSize)
	page = max(0, min(page, pages-1))
	var text strings.Builder
	fmt.Fprintf(&text, resources.Get("telegram.music.heading"), musicLabel(s.query, 180))
	rows := make([][]map[string]any, 0)
	button := func(label, action, value string) map[string]any {
		return map[string]any{"text": label, "callback_data": "music:" + s.id + ":" + action + ":" + value}
	}
	if len(s.tracks) == 0 {
		text.WriteString(resources.Get("music.empty"))
	}
	for i := page * musicPageSize; i < min(len(s.tracks), (page+1)*musicPageSize); i++ {
		track := s.tracks[i]
		duration := "—"
		if track.Live {
			duration = resources.Get("music.duration.live")
		} else if track.Duration > 0 {
			seconds := int(track.Duration.Seconds())
			duration = fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
		}
		fmt.Fprintf(&text, "%d. %s — %s · %s\n", i+1, musicLabel(track.Title, 100), musicLabel(track.Author, 60), duration)
		rows = append(rows, []map[string]any{button(fmt.Sprintf("🎵 %d. %s", i+1, musicLabel(track.Title, 48)), "get", strconv.Itoa(i))})
	}
	nav := make([]map[string]any, 0, 2)
	if page > 0 {
		nav = append(nav, button("◀", "page", strconv.Itoa(page-1)))
	}
	if page+1 < pages {
		nav = append(nav, button("▶", "page", strconv.Itoa(page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	if s.source != "" {
		for _, pair := range [][]string{{"yt", "ytm"}, {"sc", "bc"}} {
			row := make([]map[string]any, 0, 2)
			for _, source := range pair {
				label := map[string]string{"yt": "YouTube", "ytm": "YouTube Music", "sc": "SoundCloud", "bc": "Bandcamp"}[source]
				if source == s.source {
					label = "✓ " + label
				}
				row = append(row, button(label, "source", source))
			}
			rows = append(rows, row)
		}
	}
	fmt.Fprintf(&text, resources.Get("telegram.music.footer"), page+1, pages)
	return text.String(), map[string]any{"inline_keyboard": rows}
}

func (b *Bot) handleMusicCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	answer := func(key string) error {
		text := ""
		if key != "" {
			text = resources.Get(key)
		}
		return b.telegram.AnswerCallback(ctx, callback.ID, text, "", key != "")
	}
	parts := strings.Split(callback.Data, ":")
	if len(parts) != 4 || callback.Message == nil {
		return answer("telegram.music.expired")
	}
	m := b.telegramMusic()
	s, ok := m.get(parts[1], callback.Message.Chat.ID, callback.From.ID, callback.Message.MessageID)
	if !ok {
		return answer("telegram.music.expired")
	}
	index, indexErr := strconv.Atoi(parts[3])
	switch parts[2] {
	case "page":
		if indexErr != nil || index < 0 || index >= (len(s.tracks)+musicPageSize-1)/musicPageSize {
			return answer("telegram.music.expired")
		}
		if err := answer(""); err != nil {
			return err
		}
		text, keyboard := s.page(index)
		return b.telegram.EditMessageText(ctx, s.chatID, s.messageID, text, keyboard)
	case "get":
		if indexErr != nil || index < 0 || index >= len(s.tracks) {
			return answer("telegram.music.expired")
		}
	case "source":
		if s.source == "" || (parts[3] != "yt" && parts[3] != "ytm" && parts[3] != "sc" && parts[3] != "bc") {
			return answer("telegram.music.expired")
		}
		if parts[3] == s.source {
			return answer("")
		}
	default:
		return answer("telegram.music.expired")
	}
	if !m.begin() {
		return answer("telegram.music.busy")
	}
	defer m.finish()
	if err := answer(""); err != nil {
		return err
	}
	message := &telegram.Message{Chat: callback.Message.Chat, MessageID: s.replyTo, From: &callback.From}
	if parts[2] == "get" {
		return b.sendMusic(ctx, message, s.tracks[index])
	}
	tracks, err := m.search(ctx, parts[3]+":"+s.query, 20)
	if err != nil {
		log.Printf("[telegram music] source search failed: %v", err)
		_, sendErr := b.telegram.SendMessage(ctx, s.chatID, resources.Get("telegram.music.search_failed"), s.replyTo, nil)
		return sendErr
	}
	next, err := newTelegramMusicSearch(s.chatID, s.userID, s.messageID, s.replyTo, s.query, parts[3], tracks)
	if err != nil {
		return err
	}
	text, keyboard := next.page(0)
	m.put(next)
	if err := b.telegram.EditMessageText(ctx, next.chatID, next.messageID, text, keyboard); err != nil {
		m.remove(next.id)
		return err
	}
	m.remove(s.id) // An old button must never select a different track after a source change.
	return nil
}

func (b *Bot) sendMusic(ctx context.Context, message *telegram.Message, track music.Track) error {
	replyCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	status, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("telegram.music.downloading"), message.MessageID, nil)
	if err != nil {
		return err
	}
	_ = b.telegram.SendChatAction(ctx, message.Chat.ID, "upload_audio")
	audio, err := b.telegramMusic().download(ctx, track)
	if err == nil {
		_, err = b.telegram.SendUpload(ctx, message.Chat.ID, telegram.Upload{Kind: "audio", Name: audio.Name, MIME: audio.MIME, Data: audio.Data,
			Title: track.Title, Performer: track.Author, Duration: int(track.Duration.Seconds())}, message.MessageID)
	}
	if err != nil {
		key := "telegram.music.download_failed"
		if errors.Is(err, downloader.ErrMusicTooLong) {
			key = "telegram.music.too_long"
		}
		if errors.Is(err, downloader.ErrMusicTooLarge) {
			key = "telegram.music.too_large"
		}
		log.Printf("[telegram music] delivery failed: %v", err)
		return b.telegram.EditMessageText(replyCtx, message.Chat.ID, status.MessageID, resources.Get(key), nil)
	}
	return b.telegram.DeleteMessage(ctx, message.Chat.ID, status.MessageID)
}
