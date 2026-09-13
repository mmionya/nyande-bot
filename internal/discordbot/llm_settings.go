package discordbot

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/resources"
)

type llmSettings struct {
	mu       sync.RWMutex
	path     string
	disabled map[string]bool
}

func loadLLMSettings(path string) (*llmSettings, error) {
	if strings.TrimSpace(path) == "" {
		path = ".nyande-discord-llm.json"
	}
	s := &llmSettings{path: path, disabled: make(map[string]bool)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.disabled); err != nil {
		return nil, err
	}
	if s.disabled == nil {
		s.disabled = make(map[string]bool)
	}
	return s, nil
}

func llmScope(message *discordgo.Message) string {
	if message.GuildID != "" {
		return "guild:" + message.GuildID
	}
	return "dm:" + message.ChannelID
}

func (s *llmSettings) isDisabled(scope string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled[scope]
}

func (s *llmSettings) setDisabled(scope string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]bool, len(s.disabled)+1)
	for key, value := range s.disabled {
		next[key] = value
	}
	if disabled {
		next[scope] = true
	} else {
		delete(next, scope)
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path+".tmp", data, 0600); err != nil {
		return err
	}
	if err := os.Rename(s.path+".tmp", s.path); err != nil {
		return err
	}
	s.disabled = next
	return nil
}

func (b *Bot) configureLLM(session *discordgo.Session, message *discordgo.Message) string {
	fields := strings.Fields(message.Content)
	if len(fields) == 1 {
		if b.llm == nil || !b.llm.Enabled() || b.llmSettings.isDisabled(llmScope(message)) {
			return resources.Get("discord_llm_disabled")
		}
		return resources.Get("discord_llm_enabled")
	}
	if len(fields) != 2 || (strings.ToLower(fields[1]) != "off" && strings.ToLower(fields[1]) != "on") {
		return resources.Format("discord_llm_usage", map[string]any{"prefix": b.cfg.DiscordPrefix})
	}
	if message.GuildID != "" {
		if message.Author == nil {
			return resources.Get("discord_llm_admin_only")
		}
		permissions, err := session.UserChannelPermissions(message.Author.ID, message.ChannelID)
		if err != nil {
			log.Printf("[discord] LLM permissions check failed: %v", err)
			return resources.Get("discord_llm_settings_failed")
		}
		if permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer) == 0 {
			return resources.Get("discord_llm_admin_only")
		}
	}
	disabled := strings.EqualFold(fields[1], "off")
	if !disabled && (b.llm == nil || !b.llm.Enabled()) {
		return resources.Get("discord_llm_global_disabled")
	}
	if b.llmSettings == nil {
		return resources.Get("discord_llm_settings_failed")
	}
	if err := b.llmSettings.setDisabled(llmScope(message), disabled); err != nil {
		log.Printf("[discord] save LLM settings failed: %v", err)
		return resources.Get("discord_llm_settings_failed")
	}
	log.Printf("[discord] llm_settings scope=%s enabled=%t", llmScope(message), !disabled)
	if disabled {
		return resources.Get("discord_llm_disabled")
	}
	return resources.Get("discord_llm_enabled")
}
