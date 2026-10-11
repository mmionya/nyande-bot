package discordbot

import (
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/internal/access"
	"github.com/mmionya/nyande-bot/resources"
)

func (b *Bot) botBanCommand(session *discordgo.Session, message *discordgo.Message, banned bool) string {
	if message.Author == nil || !b.access.IsOwner(message.Author.ID) {
		return resources.Get("access.owner_only")
	}
	usage := fmt.Sprintf(resources.Get("discord.access.usage"), b.cfg.DiscordPrefix, b.cfg.DiscordPrefix)
	if len(message.MessageSnapshots) != 0 || message.MessageReference != nil && message.MessageReference.Type == discordgo.MessageReferenceTypeForward {
		return usage
	}
	fields := strings.Fields(message.Content)
	if len(fields) == 0 || len(fields) > 2 {
		return usage
	}
	var target string
	if reply := message.ReferencedMessage; reply != nil {
		if reply.Author == nil || len(reply.MessageSnapshots) != 0 {
			return usage
		}
		target = reply.Author.ID
	}
	if len(fields) == 2 {
		value := fields[1]
		if strings.HasPrefix(value, "<@") && strings.HasSuffix(value, ">") {
			value = strings.TrimPrefix(strings.TrimSuffix(value, ">"), "<@")
			value = strings.TrimPrefix(value, "!")
		}
		id, err := access.ParseUserID(value)
		if err != nil || target != "" && target != id {
			return usage
		}
		target = id
	}
	id, err := access.ParseUserID(target)
	if err != nil {
		return usage
	}
	if b.access.IsOwner(id) || session.State != nil && session.State.User != nil && session.State.User.ID == id {
		return resources.Get("access.protected")
	}
	if err := b.access.SetBanned(id, banned); err != nil {
		log.Printf(resources.Get("discord.log.ban_save_failed"), id, err)
		return resources.Get("discord.access.save_failed")
	}
	if banned {
		return fmt.Sprintf(resources.Get("discord.access.banned"), id)
	}
	return fmt.Sprintf(resources.Get("discord.access.unbanned"), id)
}
