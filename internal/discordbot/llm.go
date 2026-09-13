package discordbot

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/config"
	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/resources"
)

type languageModel interface {
	Enabled() bool
	Ask(context.Context, llm.Request) (string, error)
	Reset(int64) bool
}

type conversation struct {
	mu          sync.Mutex
	lastRequest time.Time
}

func discordLLMConfig(cfg config.Config) config.Config {
	// Separate clients must never overwrite each other's persisted histories.
	if strings.TrimSpace(cfg.LLMHistoryFile) != "" {
		cfg.LLMHistoryFile += ".discord"
	}
	return cfg
}

func (b *Bot) conversation(channelID string) *conversation {
	value, _ := b.conversations.LoadOrStore(channelID, &conversation{})
	return value.(*conversation)
}

func (b *Bot) shouldAnswerLLM(session *discordgo.Session, message *discordgo.Message) bool {
	if b.llm == nil || !b.llm.Enabled() || b.llmSettings.isDisabled(llmScope(message)) || strings.TrimSpace(message.Content) == "" {
		return false
	}
	if _, command := parseCommand(message.Content, b.cfg.DiscordPrefix); command {
		return false
	}
	if message.GuildID == "" {
		return true
	}
	if session.State != nil && session.State.User != nil {
		id := session.State.User.ID
		for _, user := range message.Mentions {
			if user != nil && user.ID == id {
				return true
			}
		}
		if replied := message.ReferencedMessage; replied != nil && replied.Author != nil && replied.Author.ID == id {
			return true
		}
	}
	text := strings.ToLower(message.Content)
	for _, trigger := range b.cfg.LLMTriggerWords {
		if word := strings.ToLower(strings.TrimSpace(trigger)); word != "" && strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func (b *Bot) handleLLM(session *discordgo.Session, message *discordgo.Message) {
	if !b.shouldAnswerLLM(session, message) {
		return
	}
	// Finish in-flight replies before acknowledging a settings change.
	if b.llmSettings != nil {
		b.llmSettings.mu.RLock()
		defer b.llmSettings.mu.RUnlock()
		if b.llmSettings.disabled[llmScope(message)] {
			return
		}
	}
	chatID, err := strconv.ParseInt(message.ChannelID, 10, 64)
	if err != nil {
		log.Printf("[discord] invalid LLM channel ID: %v", err)
		return
	}
	conversation := b.conversation(message.ChannelID)
	conversation.mu.Lock()
	defer conversation.mu.Unlock()
	if b.currentContext().Err() != nil || time.Since(conversation.lastRequest) < b.cfg.LLMCooldown {
		return
	}
	conversation.lastRequest = time.Now()
	timeout := b.cfg.LLMTimeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(b.currentContext(), timeout)
	defer cancel()
	_ = session.ChannelTyping(message.ChannelID, discordgo.WithContext(ctx))
	text := message.Content
	if session.State != nil && session.State.User != nil {
		id := session.State.User.ID
		text = strings.ReplaceAll(strings.ReplaceAll(text, "<@"+id+">", ""), "<@!"+id+">", "")
	}
	if strings.TrimSpace(text) == "" {
		text = resources.Get("discord_llm_greeting")
	}
	answer, err := b.llm.Ask(ctx, llm.Request{ChatID: chatID, UserName: message.Author.Username, Text: text})
	if err != nil {
		log.Printf("[discord] LLM request failed channel=%s: %v", message.ChannelID, err)
		answer = resources.Get("discord_llm_failed")
	} else if strings.TrimSpace(answer) == "" {
		answer = resources.Get("discord_llm_failed")
	}
	if err := sendLLMReply(session, message, answer); err != nil {
		log.Printf("[discord] LLM reply failed channel=%s: %v", message.ChannelID, err)
	}
}

func (b *Bot) resetLLM(message *discordgo.Message) string {
	if b.llm == nil || !b.llm.Enabled() {
		return resources.Get("discord_llm_disabled")
	}
	chatID, err := strconv.ParseInt(message.ChannelID, 10, 64)
	if err != nil {
		return resources.Get("discord_llm_failed")
	}
	conversation := b.conversation(message.ChannelID)
	conversation.mu.Lock()
	defer conversation.mu.Unlock()
	conversation.lastRequest = time.Time{}
	if b.llm.Reset(chatID) {
		return resources.Get("discord_reset_done")
	}
	return resources.Get("discord_reset_empty")
}

func sendLLMReply(session *discordgo.Session, message *discordgo.Message, text string) error {
	for index, part := range splitDiscordText(text) {
		payload := &discordgo.MessageSend{Content: part, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}
		if index == 0 {
			payload.Reference = message.SoftReference()
		}
		if _, err := session.ChannelMessageSendComplex(message.ChannelID, payload); err != nil {
			return err
		}
	}
	return nil
}

// Discord counts supplementary Unicode characters as two UTF-16 code units.
func splitDiscordText(text string) []string {
	var parts []string
	start, units := 0, 0
	for index, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > 2000 {
			parts = append(parts, text[start:index])
			start, units = index, 0
		}
		units += width
	}
	if start < len(text) {
		parts = append(parts, text[start:])
	}
	return parts
}
