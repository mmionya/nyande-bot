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

	"github.com/mmionya/nyande-bot/internal/chatlog"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/memory"
	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

type adminKey struct {
	ChatID int64
	UserID int64
}

type Bot struct {
	cfg        config.Config
	telegram   *telegram.Client
	downloader *downloader.Downloader
	llm        *llm.Client
	memories   *memory.Store
	state      *State
	allowlist  *allowlist
	linkConfig *linkDeletionSettings
	identity   telegram.User
	media      *mediaCache
	chatlog    *chatlog.Store
	reminders  *reminders.Store

	adminMu         sync.Mutex
	adminCache      map[adminKey]adminEntry
	tttMu           sync.Mutex
	tttGames        map[int64]*ticTacToeGame
	tttPending      map[int64][2]int64
	checkersMu      sync.Mutex
	checkers        map[int64]*checkersGame
	checkersPending map[int64][2]int64
	wordleMu        sync.Mutex
	wordleGames     map[wordleKey]*wordleGame
}

func New(cfg config.Config) (*Bot, error) {
	allowed, err := loadAllowlist(cfg.AllowedLinksFile)
	if err != nil {
		return nil, fmt.Errorf("load allowed links: %w", err)
	}
	linkConfig, err := loadLinkDeletionSettings(cfg.LinkModerationFile)
	if err != nil {
		return nil, fmt.Errorf("load link moderation settings: %w", err)
	}
	var memories *memory.Store
	if cfg.LLMMemoryEnabled {
		memories, err = memory.Open(cfg.LLMMemoryFile, cfg.LLMMemoryMax)
		if err != nil {
			return nil, fmt.Errorf("open long-term memory: %w", err)
		}
	}
	var chatlogStore *chatlog.Store
	if cfg.ChatLogEnabled {
		chatlogStore, err = chatlog.Open(cfg.ChatLogFile, cfg.ChatLogMaxPerChat)
		if err != nil {
			if memories != nil {
				_ = memories.Close()
			}
			return nil, fmt.Errorf("open chat log database: %w", err)
		}
	}
	remindersStore, err := reminders.Open(cfg.ReminderDBFile)
	if err != nil {
		if memories != nil {
			_ = memories.Close()
		}
		if chatlogStore != nil {
			_ = chatlogStore.Close()
		}
		return nil, fmt.Errorf("open reminders database: %w", err)
	}
	return &Bot{
		cfg: cfg, telegram: telegram.New(cfg.BotToken), downloader: downloader.New(cfg),
		llm: llm.New(cfg), memories: memories, chatlog: chatlogStore, reminders: remindersStore, state: NewState(), allowlist: allowed, linkConfig: linkConfig, media: newMediaCache(512),
		adminCache: make(map[adminKey]adminEntry),
		tttGames:   make(map[int64]*ticTacToeGame), tttPending: make(map[int64][2]int64),
		checkers: make(map[int64]*checkersGame), checkersPending: make(map[int64][2]int64),
		wordleGames: make(map[wordleKey]*wordleGame),
	}, nil
}

func (b *Bot) Close() error {
	var errs []error
	if b.memories != nil {
		if err := b.memories.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.chatlog != nil {
		if err := b.chatlog.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.reminders != nil {
		if err := b.reminders.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *Bot) Run(ctx context.Context) error {
	identity, err := b.telegram.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get bot identity: %w", err)
	}
	b.identity = identity
	log.Printf("[bot] started as @%s (%d), %s", identity.Username, identity.ID, b.cfg.String())
	go b.reminderLoop(ctx)
	commands := []map[string]string{
		{"command": "help", "description": "возможности бота"},
		{"command": "linkdelete", "description": "удаление неподдерживаемых ссылок"},
		{"command": "ttt", "description": "крестики-нолики в чате"},
		{"command": "checkers", "description": "шашки в чате"},
		{"command": "wordle", "description": "английское слово дня в чате"},
		{"command": "donate", "description": "поддержать бота"},
		{"command": "stats", "description": "статистика"},
		{"command": "reset", "description": "очистить историю диалога"},
		{"command": "remember", "description": "запомнить факт надолго"},
		{"command": "memory", "description": "показать долговременную память"},
		{"command": "forget", "description": "забыть один факт"},
		{"command": "forget_all", "description": "очистить долговременную память"},
		{"command": "ping", "description": "проверить бота"},
		{"command": "round", "description": "конвертировать видео в кружочек"},
		{"command": "voice", "description": "конвертировать в голосовое сообщение"},
		{"command": "gif", "description": "конвертировать видео в гифку"},
		{"command": "mediainfo", "description": "информация о фото/видео/аудио"},
	}
	if err := b.telegram.SetCommands(ctx, commands); err != nil {
		log.Printf("[bot] could not set commands: %v", err)
	}

	var offset int64
	semaphore := make(chan struct{}, 24)
	for {
		updates, err := b.telegram.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[bot] polling failed: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			semaphore <- struct{}{}
			go func(current telegram.Update) {
				defer func() { <-semaphore }()
				if err := b.HandleUpdate(ctx, current); err != nil {
					log.Printf("[bot] update %d failed: %v", current.UpdateID, err)
				}
			}(update)
		}
	}
}

func (b *Bot) HandleUpdate(ctx context.Context, update telegram.Update) error {
	if update.EditedMessage != nil {
		b.saveChatMessage(ctx, update.EditedMessage)
	}
	switch {
	case update.CallbackQuery != nil:
		return b.handleCallback(ctx, update.CallbackQuery)
	case update.PreCheckoutQuery != nil:
		return b.handlePreCheckout(ctx, update.PreCheckoutQuery)
	case update.Message != nil:
		return b.handleMessage(ctx, update.Message)
	default:
		return nil
	}
}

func (b *Bot) handleMessage(ctx context.Context, message *telegram.Message) error {
	b.saveChatMessage(ctx, message)
	b.state.MessagesTotal.Add(1)
	b.state.TrackChat(message.Chat.ID)

	if payment := message.SuccessfulPayment; payment != nil {
		if strings.HasPrefix(payment.InvoicePayload, "nyande_donation:") {
			log.Printf("[payments] stars received user=%d amount=%d charge=%s", userID(message), payment.TotalAmount, payment.TelegramPaymentChargeID)
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID,
				resources.Format("bot.payment.thanks", map[string]any{"stars": payment.TotalAmount}),
				message.MessageID, nil)
			return err
		}
	}

	text := strings.TrimSpace(message.ContentText())
	if command, arguments, ok := parseCommand(text); ok {
		b.state.CommandsUsed.Add(1)
		switch command {
		case "start":
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("bot.start"), message.MessageID, nil)
			return err
		case "help":
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("bot.help"), message.MessageID, nil)
			return err
		case "ping":
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("bot.ping"), message.MessageID, nil)
			return err
		case "reset":
			key := "bot.reset.empty"
			if b.llm.Reset(message.Chat.ID) {
				key = "bot.reset.done"
			}
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get(key), message.MessageID, nil)
			return err
		case "remember":
			return b.rememberCommand(ctx, message, arguments)
		case "memory":
			return b.memoryCommand(ctx, message)
		case "forget":
			return b.forgetCommand(ctx, message, arguments)
		case "forget_all":
			return b.forgetAllCommand(ctx, message)
		case "stats":
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, b.statsText(), message.MessageID, nil)
			return err
		case "donate":
			return b.sendDonationMenu(ctx, message)
		case "paysupport":
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("bot.payment.support"), message.MessageID, nil)
			return err
		case "allowlink":
			return b.addAllowedLink(ctx, message, arguments)
		case "linkdelete":
			return b.configureLinkDeletion(ctx, message, arguments)
		case "ttt":
			return b.startTicTacToe(ctx, message)
		case "checkers":
			return b.startCheckers(ctx, message)
		case "wordle":
			return b.startWordle(ctx, message)
		case "round":
			return b.roundCommand(ctx, message)
		case "voice":
			return b.voiceCommand(ctx, message)
		case "gif":
			return b.gifCommand(ctx, message)
		case "mediainfo":
			return b.mediainfoCommand(ctx, message)
		default:
			return nil
		}
	}

	urls := extractURLs(message)
	if len(urls) > 0 {
		if err := b.handleLinks(ctx, message, urls); err != nil {
			return err
		}
		return nil
	}
	if handled, err := b.handleWordleGuess(ctx, message, text); handled {
		return err
	}
	return b.handleLLM(ctx, message)
}

func parseCommand(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", "", false
	}
	command := strings.TrimPrefix(strings.SplitN(fields[0], "@", 2)[0], "/")
	arguments := ""
	if len(fields) > 1 {
		arguments = strings.Join(fields[1:], " ")
	}
	return strings.ToLower(command), arguments, true
}

func (b *Bot) handleLinks(ctx context.Context, message *telegram.Message, urls []string) error {
	forbidden := false
	for _, value := range urls {
		if !downloader.AllowedHost(value) && !b.allowlist.Allows(value) {
			forbidden = true
			break
		}
	}
	admin := false
	if forbidden && !message.IsPrivate() {
		var err error
		admin, err = b.isAdmin(ctx, message)
		if err != nil {
			return fmt.Errorf("check link author role: %w", err)
		}
	}
	if forbidden && !message.IsPrivate() && !admin && b.linkDeletionEnabled(message.Chat.ID) {
		if err := b.telegram.DeleteMessage(ctx, message.Chat.ID, message.MessageID); err != nil {
			return err
		}
		_, _ = b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.unsupported"), 0, nil)
		return nil
	}
	supported := supportedURLs(urls)
	if len(supported) == 0 {
		if message.IsPrivate() {
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.unsupported_private"), message.MessageID, nil)
			return err
		}
		return nil
	}
	return b.handleMediaURL(ctx, message, supported[0])
}

func (b *Bot) addAllowedLink(ctx context.Context, message *telegram.Message, argument string) error {
	admin, adminErr := b.isAdmin(ctx, message)
	if adminErr != nil {
		return adminErr
	}
	if !admin {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.admin_only"), message.MessageID, nil)
		return err
	}
	if strings.TrimSpace(argument) == "" {
		_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.usage"), message.MessageID, nil)
		return err
	}
	domain, added, err := b.allowlist.Add(argument)
	if err != nil {
		_, sendErr := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.error.invalid_domain"), message.MessageID, nil)
		return errors.Join(err, sendErr)
	}
	key := "links.exists"
	if added {
		key = "links.added"
	}
	_, err = b.telegram.SendMessage(ctx, message.Chat.ID, resources.Format(key, map[string]any{"domain": domain}), message.MessageID, nil)
	return err
}

func (b *Bot) handleCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	switch {
	case strings.HasPrefix(callback.Data, "donate:"):
		return b.handleDonationCallback(ctx, callback)
	case strings.HasPrefix(callback.Data, "ttt:") || strings.HasPrefix(callback.Data, "ttt_accept:"):
		return b.handleTicTacToeCallback(ctx, callback)
	case strings.HasPrefix(callback.Data, "chk:") || strings.HasPrefix(callback.Data, "chk_accept:"):
		return b.handleCheckersCallback(ctx, callback)
	default:
		return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
	}
}

func (b *Bot) sendDonationMenu(ctx context.Context, message *telegram.Message) error {
	amounts := []int{10, 20, 50, 100, 250, 500}
	rows := make([][]map[string]any, 0)
	for index := 0; index < len(amounts); index += 2 {
		row := []map[string]any{}
		for _, amount := range amounts[index:min(index+2, len(amounts))] {
			row = append(row, map[string]any{"text": fmt.Sprintf("%d ⭐", amount), "callback_data": fmt.Sprintf("donate:%d", amount)})
		}
		rows = append(rows, row)
	}
	if strings.HasPrefix(b.cfg.DonateURL, "https://") || strings.HasPrefix(b.cfg.DonateURL, "http://") {
		rows = append(rows, []map[string]any{{"text": resources.Get("telegram.donate.kofi"), "url": b.cfg.DonateURL}})
	}
	_, err := b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("telegram.donate.menu"), message.MessageID,
		map[string]any{"inline_keyboard": rows})
	return err
}

func (b *Bot) handleDonationCallback(ctx context.Context, callback *telegram.CallbackQuery) error {
	if callback.Message == nil {
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("bot.callback.private_only"), "", true)
	}
	amount, _ := strconv.Atoi(strings.TrimPrefix(callback.Data, "donate:"))
	if !containsInt([]int{10, 20, 50, 100, 250, 500}, amount) {
		return b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("bot.callback.invalid_amount"), "", true)
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	payload := fmt.Sprintf("nyande_donation:%d:%d:%s", amount, callback.From.ID, hex.EncodeToString(nonce))
	if err := b.telegram.SendInvoice(ctx, callback.Message.Chat.ID,
		resources.Get("telegram.donate.invoice_title"), resources.Get("telegram.donate.description"), payload, amount); err != nil {
		_ = b.telegram.AnswerCallback(ctx, callback.ID, resources.Get("bot.callback.invoice_failed"), "", true)
		return err
	}
	return b.telegram.AnswerCallback(ctx, callback.ID, "", "", false)
}

func (b *Bot) handlePreCheckout(ctx context.Context, query *telegram.PreCheckoutQuery) error {
	parts := strings.Split(query.InvoicePayload, ":")
	valid := len(parts) == 4 && parts[0] == "nyande_donation" && query.Currency == "XTR"
	amount, expectedUser := 0, int64(0)
	if valid {
		amount, _ = strconv.Atoi(parts[1])
		expectedUser, _ = strconv.ParseInt(parts[2], 10, 64)
		valid = containsInt([]int{10, 20, 50, 100, 250, 500}, amount) &&
			query.TotalAmount == amount && query.From.ID == expectedUser
	}
	errorMessage := ""
	if !valid {
		errorMessage = resources.Get("bot.payment.invalid")
	}
	return b.telegram.AnswerPreCheckout(ctx, query.ID, valid, errorMessage)
}

func (b *Bot) statsText() string {
	uptime := time.Since(b.state.StartedAt).Round(time.Second)
	total := b.state.MediaTotal.Load()
	errorsCount := b.state.MediaErrors.Load()
	errorRate := float64(0)
	if total > 0 {
		errorRate = float64(errorsCount) / float64(total) * 100
	}
	return resources.Format("bot.stats", map[string]any{
		"uptime": uptime, "chats": b.state.UniqueChats(),
		"messages": b.state.MessagesTotal.Load(), "commands": b.state.CommandsUsed.Load(),
		"media": total, "success": total - errorsCount, "errors": errorsCount,
		"error_rate": fmt.Sprintf("%.1f", errorRate),
		"llm_calls":  b.state.LLMCalls.Load(), "llm_errors": b.state.LLMErrors.Load(),
	})
}

func userID(message *telegram.Message) int64 {
	if message.From == nil {
		return 0
	}
	return message.From.ID
}

func (b *Bot) saveChatMessage(ctx context.Context, message *telegram.Message) {
	if b.chatlog == nil || message == nil {
		return
	}
	text := strings.TrimSpace(message.ContentText())
	if text == "" {
		return
	}
	var currentUserID int64
	var username, displayName string
	if message.From != nil {
		currentUserID = message.From.ID
		username = message.From.Username
		displayName = message.From.DisplayName()
	}
	date := time.Now().UTC()
	if message.Date > 0 {
		date = time.Unix(message.Date, 0).UTC()
	}
	if err := b.chatlog.Save(ctx, chatlog.Message{
		ChatID:      message.Chat.ID,
		MessageID:   message.MessageID,
		UserID:      currentUserID,
		Username:    username,
		DisplayName: displayName,
		Text:        text,
		Date:        date,
	}); err != nil {
		log.Printf("[chatlog] save chat=%d msg=%d failed: %v", message.Chat.ID, message.MessageID, err)
	}
}

func (b *Bot) reminderLoop(ctx context.Context) {
	if b.reminders == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			due, err := b.reminders.GetDue(ctx, now)
			if err != nil {
				log.Printf("[reminders] get due failed: %v", err)
				continue
			}
			for _, r := range due {
				userTag := r.UserName
				if userTag == "" {
					userTag = "друг"
				}
				text := fmt.Sprintf("⏰ **Напоминание для %s**:\n%s", userTag, r.Text)
				_, sendErr := b.telegram.SendMessage(ctx, r.ChatID, text, r.MessageID, nil)
				if sendErr != nil {
					log.Printf("[reminders] send reminder %d failed: %v", r.ID, sendErr)
				}
				if err := b.reminders.MarkCompleted(ctx, r.ID); err != nil {
					log.Printf("[reminders] mark completed %d failed: %v", r.ID, err)
				}
			}
		}
	}
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
