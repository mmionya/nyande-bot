package discordbot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mmionya/nyande-bot/resources"
)

// Keep /nyande as a compatible alias, while exposing each command in Discord's
// native slash picker with its own description and typed options.
func discordApplicationCommands() []*discordgo.ApplicationCommand {
	music := musicApplicationCommand()
	commands := []*discordgo.ApplicationCommand{music}
	for _, sub := range music.Options {
		if sub.Name == "help" {
			continue
		}
		commands = append(commands, &discordgo.ApplicationCommand{Name: sub.Name, Description: sub.Description, Options: sub.Options, DMPermission: music.DMPermission})
	}
	commands = append(commands,
		&discordgo.ApplicationCommand{Name: "help", Description: "Все команды и возможности бота"},
		&discordgo.ApplicationCommand{Name: "ping", Description: "Проверить, работает ли бот"},
		&discordgo.ApplicationCommand{Name: "stats", Description: "Статистика бота"},
		&discordgo.ApplicationCommand{Name: "reset", Description: "Очистить историю диалога с LLM"},
		&discordgo.ApplicationCommand{Name: "llm", Description: "Включить, выключить или проверить LLM", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "mode", Description: "Действие; без параметра — текущий статус", Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "Статус", Value: "status"}, {Name: "Включить", Value: "on"}, {Name: "Выключить", Value: "off"}}}}},
		&discordgo.ApplicationCommand{Name: "gif", Description: "Сделать GIF из видео по ссылке или из вложения", Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "url", Description: "Ссылка на видео", MaxLength: 2000}, {Type: discordgo.ApplicationCommandOptionAttachment, Name: "video", Description: "Видеофайл для преобразования"}}},
	)
	return commands
}

func (b *Bot) registerDiscordCommands() error {
	if b.session.State == nil || b.session.State.User == nil {
		return errors.New("Discord identity unavailable")
	}
	var errs []error
	for _, command := range discordApplicationCommands() {
		// Upsert our commands without deleting unrelated application commands.
		ctx, cancel := context.WithTimeout(b.currentContext(), 5*time.Second)
		_, err := b.session.ApplicationCommandCreate(b.session.State.User.ID, "", command, discordgo.WithContext(ctx))
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("/%s: %w", command.Name, err))
		}
	}
	return errors.Join(errs...)
}

func standaloneMusicCommand(name string) bool {
	for _, sub := range musicApplicationCommand().Options {
		if sub.Name == name && name != "help" {
			return true
		}
	}
	return false
}

func (b *Bot) handleGeneralSlash(s *discordgo.Session, i *discordgo.Interaction) bool {
	data := i.ApplicationCommandData()
	switch data.Name {
	case "help", "ping", "stats", "reset", "llm", "gif":
	default:
		return false
	}
	if musicActor(i) == nil {
		musicPrivate(s, i, "Не удалось определить автора команды.")
		return true
	}
	if !musicDefer(s, i, false) {
		return true
	}
	b.commands.Add(1)
	if data.Name == "gif" {
		b.slashGIF(s, i, data)
		return true
	}
	message := musicInteractionMessage(i, data.Name, "")
	var response string
	switch data.Name {
	case "help":
		response = resources.Get("discord_slash_help")
	case "ping":
		response = resources.Get("discord_ping")
	case "stats":
		response = b.statsText()
	case "reset":
		response = b.resetLLM(message)
	case "llm":
		for _, option := range data.Options {
			if option.Name == "mode" && option.StringValue() != "status" {
				message.Content += " " + option.StringValue()
			}
		}
		response = b.configureLLM(s, message)
	}
	musicEdit(s, i, &discordgo.InteractionResponseData{Content: response})
	return true
}

func (b *Bot) slashGIF(s *discordgo.Session, i *discordgo.Interaction, data discordgo.ApplicationCommandInteractionData) {
	message := musicInteractionMessage(i, "gif", "")
	for _, option := range data.Options {
		switch option.Name {
		case "url":
			message.Content += " " + option.StringValue()
		case "video":
			if data.Resolved != nil {
				if attachment := data.Resolved.Attachments[fmt.Sprint(option.Value)]; attachment != nil {
					message.Attachments = append(message.Attachments, attachment)
				}
			}
		}
	}
	source := gifSource(message)
	if source == "" {
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: "Укажи ссылку в url или прикрепи видео в video."})
		return
	}
	// Acknowledge first, then wait for the shared media concurrency limit.
	ctx, cancel := context.WithTimeout(b.currentContext(), downloadTimeout)
	defer cancel()
	select {
	case b.semaphore <- struct{}{}:
		defer func() { <-b.semaphore }()
	case <-ctx.Done():
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("discord_gif_timeout")})
		return
	}
	b.mediaTotal.Add(1)
	fail := func(text string, err error) {
		b.mediaErrors.Add(1)
		log.Printf("[discord] slash gif failed: %v", err)
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: text})
	}
	result, err := b.downloader.Download(ctx, source)
	if err != nil {
		fail(downloadErrorText(err), err)
		return
	}
	var video []byte
	for _, item := range result.Items {
		if item.Kind == "video" || item.Kind == "animation" {
			video = item.Data
			break
		}
	}
	if video == nil {
		fail(resources.Get("discord_gif_no_video"), errors.New("no video"))
		return
	}
	gif, err := convertGIF(ctx, video, b.cfg.MaxFileSize)
	if err != nil {
		key := "discord_gif_failed"
		if errors.Is(err, errGIFTooLarge) {
			key = "discord_gif_too_large"
		} else if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			key = "discord_gif_timeout"
		}
		fail(resources.Get(key), err)
		return
	}
	content := ""
	if _, err = s.InteractionResponseEdit(i, &discordgo.WebhookEdit{Content: &content, AllowedMentions: noMusicMentions(), Files: []*discordgo.File{{Name: resources.Get("discord_gif_filename"), ContentType: "image/gif", Reader: bytes.NewReader(gif)}}}, discordgo.WithContext(ctx)); err != nil {
		fail(resources.Get("discord_send_failed"), err)
	}
}
