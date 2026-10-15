package bot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/internal/access"
	"github.com/mmionya/nyande-bot/internal/chatlog"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/downloader"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/memory"
	"github.com/mmionya/nyande-bot/internal/quotes"
	"github.com/mmionya/nyande-bot/internal/reminders"
	"github.com/mmionya/nyande-bot/internal/schedules"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

const silentModerationTrigger = "blahajblahajblahaj"

type adminKey struct {
	ChatID int64
	UserID int64
}

type Bot struct {
	cfg        config.Config
	access     *access.Store
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
	schedules  *schedules.Store
	quotes     *quotes.Store

	updateLocks     sync.Map
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
	banPath := cfg.BannedUsersFile
	if banPath == "" {
		banPath = ".nyande-banned-users.json"
	}
	users, err := access.Open(banPath, cfg.BotOwnerIDs)
	if err != nil {
		return nil, fmt.Errorf(resources.Get("telegram.error.load_bans"), err)
	}
	allowed, err := loadAllowlist(cfg.AllowedLinksFile)
	if err != nil {
		return nil, fmt.Errorf(resources.Get("telegram.error.load_allowed_links"), err)
	}
	linkConfig, err := loadLinkDeletionSettings(cfg.LinkModerationFile)
	if err != nil {
		return nil, fmt.Errorf(resources.Get("telegram.error.load_moderation"), err)
	}
	var memories *memory.Store
	if cfg.LLMMemoryEnabled {
		memories, err = memory.Open(cfg.LLMMemoryFile, cfg.LLMMemoryMax)
		if err != nil {
			return nil, fmt.Errorf(resources.Get("telegram.error.open_memory"), err)
		}
	}
	var chatlogStore *chatlog.Store
	if cfg.ChatLogEnabled {
		chatlogStore, err = chatlog.Open(cfg.ChatLogFile, cfg.ChatLogMaxPerChat, cfg.StorageEncryptionKey)
		if err != nil {
			if memories != nil {
				_ = memories.Close()
			}
			return nil, fmt.Errorf(resources.Get("chatlog.error.open_chat_log_database"), err)
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
		return nil, fmt.Errorf(resources.Get("reminders.error.open_reminders_database"), err)
	}
	quoteStore, err := quotes.Open(cfg.QuoteDBFile)
	if err != nil {
		_ = remindersStore.Close()
		if memories != nil {
			_ = memories.Close()
		}
		if chatlogStore != nil {
			_ = chatlogStore.Close()
		}
		return nil, fmt.Errorf(resources.Get("telegram.error.open_quotes"), err)
	}
	schedulePath := cfg.ScheduleDBFile
	if schedulePath == "" {
		schedulePath = ".nyande-schedules.db"
	}
	scheduleStore, err := schedules.Open(schedulePath)
	if err != nil {
		_ = quoteStore.Close()
		_ = remindersStore.Close()
		if memories != nil {
			_ = memories.Close()
		}
		if chatlogStore != nil {
			_ = chatlogStore.Close()
		}
		return nil, fmt.Errorf(resources.Get("schedule.error.open"), err)
	}
	return &Bot{
		cfg: cfg, access: users, telegram: telegram.New(cfg.BotToken), downloader: downloader.New(cfg),
		llm: llm.New(cfg), memories: memories, chatlog: chatlogStore, reminders: remindersStore, state: NewState(), allowlist: allowed, linkConfig: linkConfig, media: newMediaCache(512),
		adminCache: make(map[adminKey]adminEntry),
		tttGames:   make(map[int64]*ticTacToeGame), tttPending: make(map[int64][2]int64),
		checkers: make(map[int64]*checkersGame), checkersPending: make(map[int64][2]int64),
		wordleGames: make(map[wordleKey]*wordleGame),
		quotes:      quoteStore,
		schedules:   scheduleStore,
	}, nil
}

func (b *Bot) Close() error {
	var errs []error
	if b.schedules != nil {
		if err := b.schedules.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.quotes != nil {
		if err := b.quotes.Close(); err != nil {
			errs = append(errs, err)
		}
	}
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
		return fmt.Errorf(resources.Get("telegram.bot.error.identity"), err)
	}
	b.identity = identity
	log.Printf(resources.Get("telegram.log.started"), identity.Username, identity.ID, b.cfg.String())
	go b.reminderLoop(ctx)
	go b.scheduleLoop(ctx)
	commands := []map[string]string{
		{"command": "help", "description": resources.Get("telegram.command.help")},
		{"command": "schedule", "description": resources.Get("telegram.command.schedule")},
		{"command": "botban", "description": resources.Get("telegram.command.botban")},
		{"command": "botunban", "description": resources.Get("telegram.command.botunban")},
		{"command": "linkdelete", "description": resources.Get("telegram.command.linkdelete")},
		{"command": "permitlinks", "description": resources.Get("telegram.command.permitlinks")},
		{"command": "revokelinks", "description": resources.Get("telegram.command.revokelinks")},
		{"command": "ttt", "description": resources.Get("telegram.command.ttt")},
		{"command": "checkers", "description": resources.Get("telegram.command.checkers")},
		{"command": "wordle", "description": resources.Get("telegram.command.wordle")},
		{"command": "quote", "description": resources.Get("telegram.command.quote")},
		{"command": "donate", "description": resources.Get("telegram.command.donate")},
		{"command": "stats", "description": resources.Get("telegram.command.stats")},
		{"command": "reset", "description": resources.Get("telegram.command.reset")},
		{"command": "remember", "description": resources.Get("telegram.command.remember")},
		{"command": "memory", "description": resources.Get("telegram.command.memory")},
		{"command": "forget", "description": resources.Get("telegram.command.forget")},
		{"command": "forget_all", "description": resources.Get("telegram.command.forget_all")},
		{"command": "ping", "description": resources.Get("telegram.command.ping")},
		{"command": "round", "description": resources.Get("telegram.command.round")},
		{"command": "voice", "description": resources.Get("telegram.command.voice")},
		{"command": "gif", "description": resources.Get("telegram.command.gif")},
		{"command": "mediainfo", "description": resources.Get("telegram.command.mediainfo")},
	}
	if err := b.telegram.SetCommands(ctx, commands); err != nil {
		log.Printf(resources.Get("telegram.log.commands_failed"), err)
	}

	var offset int64
	// ponytail: 24 shared workers bound resource use; separate worker pools
	// are needed if queued LLM requests regularly exhaust this capacity.
	semaphore := make(chan struct{}, 24)
	for {
		updates, err := b.telegram.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf(resources.Get("telegram.log.polling_failed"), err)
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
					log.Printf(resources.Get("telegram.log.update_failed"), current.UpdateID, err)
				}
			}(update)
		}
	}
}

func (b *Bot) HandleUpdate(ctx context.Context, update telegram.Update) error {
	var chatID int64
	var text string
	switch {
	case update.Message != nil:
		chatID = update.Message.Chat.ID
		text = strings.TrimSpace(update.Message.ContentText())
	case update.EditedMessage != nil:
		chatID = update.EditedMessage.Chat.ID
	case update.CallbackQuery != nil && update.CallbackQuery.Message != nil:
		chatID = update.CallbackQuery.Message.Chat.ID
	}
	// Match the supported-link branch in handleMessage. Commands and links
	// that may reach the LLM stay serialized with the conversation and /reset.
	mediaUpdate := update.Message != nil &&
		!strings.HasPrefix(text, "/") && text != silentModerationTrigger &&
		len(supportedURLs(extractURLs(update.Message))) > 0
	if mediaUpdate && b.schedules != nil && scheduleOwner(update.Message) {
		_, draftErr := b.schedules.GetDraft(ctx, update.Message.From.ID, chatID)
		if !errors.Is(draftErr, sql.ErrNoRows) {
			mediaUpdate = false
		}
	}
	unlock := b.lockUpdates(chatID, mediaUpdate)
	defer unlock()

	if blocked, err := b.rejectBlockedUpdate(ctx, update); blocked {
		return err
	}

	if update.EditedMessage != nil {
		if !b.silentModeration(update.EditedMessage.Chat.ID) {
			b.saveChatMessage(ctx, update.EditedMessage)
		}
		if b.linkDeletionEnabled(update.EditedMessage.Chat.ID) {
			return b.handleLinks(ctx, update.EditedMessage, extractURLs(update.EditedMessage), false)
		}
		return nil
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

func (b *Bot) handleMessage(ctx context.Context, message *telegram.Message) (err error) {
	if message.Chat.Type == "group" || message.Chat.Type == "supergroup" {
		if b.silentModeration(message.Chat.ID) {
			if message.IsAutomaticForward {
				return nil
			}
			if handled, err := b.handleLinkPermissionCommand(ctx, message); handled {
				return err
			}
			return b.handleLinks(ctx, message, extractURLs(message), false)
		}
		if strings.TrimSpace(message.ContentText()) == silentModerationTrigger {
			admin, err := b.isAdmin(ctx, message)
			if err != nil {
				return err
			}
			if !admin {
				return nil
			}
			if b.linkConfig == nil {
				return errors.New(resources.Get("telegram.error.link_settings_unavailable"))
			}
			unlockMedia := b.lockUpdates(message.Chat.ID, true)
			defer unlockMedia()
			if err := b.linkConfig.EnableSilent(message.Chat.ID); err != nil {
				return err
			}
			log.Printf(resources.Get("telegram.log.silent_enabled"), message.Chat.ID, userID(message), message.MessageID)
			return nil
		}
	}
	b.saveChatMessage(ctx, message)
	b.state.MessagesTotal.Add(1)
	b.state.TrackChat(message.Chat.ID)

	if payment := message.SuccessfulPayment; payment != nil {
		if strings.HasPrefix(payment.InvoicePayload, "nyande_donation:") {
			log.Printf(resources.Get("telegram.log.stars_received"), userID(message), payment.TotalAmount, payment.TelegramPaymentChargeID)
			_, err := b.telegram.SendMessage(ctx, message.Chat.ID,
				resources.Format("bot.payment.thanks", map[string]any{"stars": payment.TotalAmount}),
				message.MessageID, nil)
			return err
		}
	}

	text := strings.TrimSpace(message.ContentText())
	if strings.HasPrefix(text, "/") {
		command, arguments, ok := parseCommand(text, b.identity.Username)
		if !ok {
			return nil
		}
		started := time.Now()
		log.Printf(resources.Get("telegram.log.command_started"), message.Chat.ID, userID(message), message.MessageID, command)
		defer func() {
			log.Printf(resources.Get("telegram.log.command_finished"), message.Chat.ID, userID(message), message.MessageID, command, err != nil, time.Since(started).Milliseconds())
		}()
		b.state.CommandsUsed.Add(1)
		switch command {
		case "schedule":
			return b.scheduleCommand(ctx, message, arguments)
		case "botban", "botunban":
			unlockMedia := b.lockUpdates(message.Chat.ID, true)
			defer unlockMedia()
			return b.botBanCommand(ctx, message, command, arguments)
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
		case "quote":
			return b.quoteCommand(ctx, message, arguments)
		case "round":
			return b.roundCommand(ctx, message)
		case "voice":
			return b.voiceCommand(ctx, message)
		case "gif":
			return b.gifCommand(ctx, message)
		case "mediainfo":
			return b.mediainfoCommand(ctx, message)
		default:
			log.Printf(resources.Get("telegram.log.command_unknown"), message.Chat.ID, message.MessageID, command)
			return nil
		}
	}

	if handled, err := b.handleScheduleInput(ctx, message); handled {
		return err
	}
	urls := extractURLs(message)
	if len(urls) > 0 {
		if err := b.handleLinks(ctx, message, urls, true); err != nil {
			return err
		}
		return nil
	}
	if handled, err := b.handleQuoteTrigger(ctx, message); handled {
		return err
	}
	if handled, err := b.handleWordleGuess(ctx, message, text); handled {
		return err
	}
	return b.handleLLM(ctx, message)
}

func parseCommand(text, botUsername string) (string, string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", "", false
	}
	token, recipient, addressed := strings.Cut(fields[0], "@")
	if addressed && (botUsername == "" || !strings.EqualFold(recipient, botUsername)) {
		return "", "", false
	}
	command := strings.TrimPrefix(token, "/")
	arguments := ""
	if len(fields) > 1 {
		arguments = strings.Join(fields[1:], " ")
	}
	return strings.ToLower(command), arguments, true
}

func (b *Bot) handleLinks(ctx context.Context, message *telegram.Message, urls []string, download bool) error {
	if b.silentModeration(message.Chat.ID) {
		// This Telegram flag identifies posts from the linked discussion channel;
		// ordinary forwards and comments do not inherit the exemption.
		if message.IsAutomaticForward {
			return nil
		}
		if message.SenderChat == nil && message.From != nil && b.linkConfig.UserPermitted(message.Chat.ID, message.From.ID) {
			return nil
		}
	}
	forbidden := false
	for _, value := range urls {
		if b.silentModeration(message.Chat.ID) || (!downloader.AllowedHost(value) && !b.allowlist.Allows(value)) {
			forbidden = true
			break
		}
	}
	admin := false
	if forbidden && !message.IsPrivate() {
		var err error
		admin, err = b.isAdmin(ctx, message)
		if err != nil {
			return fmt.Errorf(resources.Get("telegram.error.link_author_role"), err)
		}
	}
	if forbidden && !message.IsPrivate() && !admin && b.linkDeletionEnabled(message.Chat.ID) {
		if err := b.telegram.DeleteMessage(ctx, message.Chat.ID, message.MessageID); err != nil {
			return err
		}
		if !b.silentModeration(message.Chat.ID) {
			_, _ = b.telegram.SendMessage(ctx, message.Chat.ID, resources.Get("links.unsupported"), 0, nil)
		}
		return nil
	}
	if !download || b.silentModeration(message.Chat.ID) {
		return nil
	}
	if b.blockedMessage(message) {
		return nil
	}
	supported := supportedURLs(urls)
	if len(supported) == 0 {
		if b.llm != nil && b.llm.Enabled() {
			return b.handleLLM(ctx, message)
		}
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
	if callback.Message != nil && b.silentModeration(callback.Message.Chat.ID) {
		return nil
	}
	switch {
	case strings.HasPrefix(callback.Data, "sch:"):
		return b.handleScheduleCallback(ctx, callback)
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
		log.Printf(resources.Get("telegram.log.chatlog_save_failed"), message.Chat.ID, message.MessageID, err)
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
				log.Printf(resources.Get("telegram.log.reminders_due_failed"), err)
				continue
			}
			for _, r := range due {
				b.deliverReminder(ctx, r)
			}
		}
	}
}

// Serialize downloads separately from conversation updates in each chat.
// Silent activation, bans and link permissions take both, normal then media.
func (b *Bot) lockUpdates(chatID int64, media bool) func() {
	key := struct {
		chatID int64
		media  bool
	}{chatID, media}
	value, _ := b.updateLocks.LoadOrStore(key, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (b *Bot) deliverReminder(ctx context.Context, r reminders.Reminder) {
	unlock := b.lockUpdates(r.ChatID, false)
	defer unlock()
	if b.silentModeration(r.ChatID) || b.access.IsBanned(strconv.FormatInt(r.UserID, 10)) {
		if err := b.reminders.MarkCompleted(ctx, r.ID); err != nil {
			log.Printf(resources.Get("telegram.log.reminder_discard_failed"), r.ID, err)
		}
		return
	}
	userTag := r.UserName
	if userTag == "" {
		userTag = resources.Get("telegram.reminder.default_user")
	}
	text := fmt.Sprintf(resources.Get("telegram.reminder.message"), userTag, r.Text)
	_, sendErr := b.telegram.SendMessage(ctx, r.ChatID, text, r.MessageID, nil)
	if sendErr != nil {
		log.Printf(resources.Get("telegram.log.reminder_send_failed"), r.ID, sendErr)
		errText := strings.ToLower(sendErr.Error())
		isPermanent := strings.Contains(errText, "kicked") ||
			strings.Contains(errText, "blocked") ||
			strings.Contains(errText, "not found") ||
			strings.Contains(errText, "deactivated") ||
			strings.Contains(errText, "migrated") ||
			time.Since(r.TriggerAt) > 5*time.Minute
		if !isPermanent {
			return
		}
	}
	if err := b.reminders.MarkCompleted(ctx, r.ID); err != nil {
		log.Printf(resources.Get("telegram.log.reminder_complete_failed"), r.ID, err)
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
