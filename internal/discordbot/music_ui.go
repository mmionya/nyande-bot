package discordbot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/disgoorg/disgolink/v4/lavalink"
	"github.com/mmionya/nyande-bot/resources"
)

const musicSearchPageSize = 10

type musicBrowser struct {
	mu                                       sync.Mutex
	id, owner, guild, channel, source, query string
	expires                                  time.Time
	tracks                                   []lavalink.Track
	selected                                 map[int]bool
	page                                     int
	busy, consumed                           bool
}
type musicPanel struct{ id, channel, nonce string }
type musicUIState struct {
	mu          sync.Mutex
	browsers    map[string]*musicBrowser
	searchSlots chan struct{}
	panelMu     sync.Mutex
	panels      map[string]musicPanel
}

var musicSources = []struct{ value, name string }{
	{"ytm", "YouTube Music"}, {"yt", "YouTube"}, {"sc", "SoundCloud"}, {"bc", "Bandcamp"},
}

func validMusicSource(source string) bool {
	for _, s := range musicSources {
		if s.value == source {
			return true
		}
	}
	return false
}
func musicSourceName(source string) string {
	for _, s := range musicSources {
		if s.value == source {
			return s.name
		}
	}
	return source
}
func musicUIID() string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}
func noMusicMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func musicApplicationCommand() *discordgo.ApplicationCommand {
	sub := func(name, description string, options ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: description, Options: options}
	}
	text := func(name, description string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: description, Required: required, MaxLength: 500}
	}
	source := text("source", "Источник музыки", false)
	for _, s := range musicSources {
		source.Choices = append(source.Choices, &discordgo.ApplicationCommandOptionChoice{Name: s.name, Value: s.value})
	}
	integer := func(name, description string, min, max float64) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: name, Description: description, Required: true, MinValue: &min, MaxValue: max}
	}
	repeat := text("mode", "Режим повтора", true)
	repeat.Choices = []*discordgo.ApplicationCommandOptionChoice{{Name: "Выключен", Value: "off"}, {Name: "Текущий трек", Value: "track"}, {Name: "Очередь", Value: "queue"}}
	dm := false
	return &discordgo.ApplicationCommand{Name: "nyande", Description: "Поиск и воспроизведение музыки", DMPermission: &dm, Options: []*discordgo.ApplicationCommandOption{
		sub("search", "Открыть поиск музыки", text("query", "Название, исполнитель или ссылка", false), source),
		sub("play", "Запустить трек или добавить в очередь", text("query", "Название или ссылка", true), source),
		sub("queue", "Посмотреть очередь"), sub("player", "Показать проигрыватель"),
		sub("skip", "Пропустить трек"), sub("pause", "Пауза"), sub("resume", "Продолжить"),
		sub("stop", "Остановить и выйти"), sub("shuffle", "Перемешать ожидающие треки"), sub("clear", "Очистить очередь, оставив текущий трек"),
		sub("move", "Переставить ожидающий трек", integer("from", "Позиция в очереди", 1, 50), integer("to", "Новая позиция", 1, 50)),
		sub("volume", "Установить громкость", integer("value", "Громкость в процентах", 0, 100)),
		sub("repeat", "Повтор трека или очереди", repeat), sub("help", "Справка по музыке"),
	}}
}
func (b *Bot) registerMusicCommands() error {
	if b.session.State == nil || b.session.State.User == nil {
		return fmt.Errorf("Discord identity unavailable")
	}
	_, err := b.session.ApplicationCommandCreate(b.session.State.User.ID, "", musicApplicationCommand())
	return err
}
func musicActor(i *discordgo.Interaction) *discordgo.User {
	if i.Member != nil {
		return i.Member.User
	}
	return i.User
}
func musicInteractionMessage(i *discordgo.Interaction, command, query string) *discordgo.Message {
	return &discordgo.Message{GuildID: i.GuildID, ChannelID: i.ChannelID, Author: musicActor(i), Content: "!" + command + " " + query}
}
func musicRespond(s *discordgo.Session, i *discordgo.Interaction, t discordgo.InteractionResponseType, data *discordgo.InteractionResponseData) bool {
	if err := s.InteractionRespond(i, &discordgo.InteractionResponse{Type: t, Data: data}); err != nil {
		log.Printf("[music] interaction response failed: %v", err)
		return false
	}
	return true
}
func musicPrivate(s *discordgo.Session, i *discordgo.Interaction, text string) {
	musicRespond(s, i, discordgo.InteractionResponseChannelMessageWithSource, &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMusicMentions()})
}
func musicDefer(s *discordgo.Session, i *discordgo.Interaction, update bool) bool {
	if update {
		return musicRespond(s, i, discordgo.InteractionResponseDeferredMessageUpdate, nil)
	}
	return musicRespond(s, i, discordgo.InteractionResponseDeferredChannelMessageWithSource, &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral})
}
func musicEdit(s *discordgo.Session, i *discordgo.Interaction, data *discordgo.InteractionResponseData) {
	if _, err := s.InteractionResponseEdit(i, &discordgo.WebhookEdit{Content: &data.Content, Embeds: &data.Embeds, Components: &data.Components, AllowedMentions: noMusicMentions()}); err != nil {
		log.Printf("[music] interaction edit failed: %v", err)
	}
}
func musicModal(s *discordgo.Session, i *discordgo.Interaction, source string) {
	musicRespond(s, i, discordgo.InteractionResponseModal, &discordgo.InteractionResponseData{CustomID: "music:modal:" + source, Title: "Поиск · " + musicSourceName(source), Components: []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{CustomID: "query", Label: "Название, исполнитель или ссылка", Style: discordgo.TextInputShort, Placeholder: "Что хочешь послушать?", Required: true, MinLength: 1, MaxLength: 500}}},
	}})
}
func musicModalQuery(data discordgo.ModalSubmitInteractionData) string {
	for _, component := range data.Components {
		row, ok := component.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, child := range row.Components {
			if input, ok := child.(*discordgo.TextInput); ok && input.CustomID == "query" {
				return strings.TrimSpace(input.Value)
			}
		}
	}
	return ""
}

func (b *Bot) handleMusicInteraction(s *discordgo.Session, event *discordgo.InteractionCreate) {
	if event == nil || event.Interaction == nil {
		return
	}
	i := event.Interaction
	if actor := musicActor(i); actor != nil && b.access.IsBanned(actor.ID) {
		if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
			musicRespond(s, i, discordgo.InteractionApplicationCommandAutocompleteResult, &discordgo.InteractionResponseData{Choices: []*discordgo.ApplicationCommandOptionChoice{}})
		} else {
			musicPrivate(s, i, "Доступ к боту заблокирован.")
		}
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		if b.handleGeneralSlash(s, i) {
			return
		}
		data := i.ApplicationCommandData()
		if data.Name != "nyande" {
			if !standaloneMusicCommand(data.Name) {
				return
			}
			// Copy the interaction so other handlers see the original command.
			copyInteraction := *i
			copyInteraction.Data = discordgo.ApplicationCommandInteractionData{Name: "nyande", Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: data.Name, Type: discordgo.ApplicationCommandOptionSubCommand, Options: data.Options}}}
			i = &copyInteraction
		}
	case discordgo.InteractionMessageComponent:
		if !strings.HasPrefix(i.MessageComponentData().CustomID, "music:") {
			return
		}
	case discordgo.InteractionModalSubmit:
		if !strings.HasPrefix(i.ModalSubmitData().CustomID, "music:") {
			return
		}
	default:
		return
	}
	if i.GuildID == "" || musicActor(i) == nil {
		musicPrivate(s, i, resources.Get("music.guild_only"))
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		options := i.ApplicationCommandData().Options
		if len(options) != 1 {
			musicPrivate(s, i, resources.Get("music.ui_expired"))
			return
		}
		command := options[0].Name
		args := map[string]string{}
		for _, option := range options[0].Options {
			args[option.Name] = fmt.Sprint(option.Value)
		}
		source := args["source"]
		if source == "" {
			source = "yt"
		}
		if !validMusicSource(source) {
			musicPrivate(s, i, resources.Get("music.query"))
			return
		}
		if command == "search" && strings.TrimSpace(args["query"]) == "" {
			musicModal(s, i, source)
			return
		}
		if !musicDefer(s, i, false) {
			return
		}
		if command == "search" {
			b.searchMusicUI(s, i, args["query"], source)
			return
		}
		if command == "help" {
			musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.ui_help")})
			return
		}
		query := args["query"]
		if command == "play" && !strings.Contains(query, "://") {
			query = source + ":" + query
		}
		switch command {
		case "volume":
			query = args["value"]
		case "repeat":
			query = args["mode"]
		case "move":
			query = args["from"] + " " + args["to"]
		}
		if command == "player" {
			m, err := b.getMusic(s)
			if err != nil {
				musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.unavailable")})
				return
			}
			g := m.guild(i.GuildID)
			g.mu.Lock()
			active := g.current != nil
			g.mu.Unlock()
			if !active {
				musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.queue_empty")})
				return
			}
			if err := b.refreshMusicPanel(s, i.GuildID, i.ChannelID, true); err != nil {
				musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.failed")})
				return
			}
			musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.ui_panel_shown")})
			return
		}
		allowed := map[string]bool{"play": true, "queue": true, "skip": true, "pause": true, "resume": true, "stop": true, "shuffle": true, "clear": true, "move": true, "volume": true, "repeat": true}
		if !allowed[command] {
			musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.ui_help")})
			return
		}
		result := b.musicCommand(s, musicInteractionMessage(i, command, query), command)
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: result})
		if command != "queue" {
			if err := b.refreshMusicPanel(s, i.GuildID, i.ChannelID, command == "play"); err != nil {
				log.Printf("[music] panel update failed: %v", err)
			}
		}
	case discordgo.InteractionModalSubmit:
		parts := strings.Split(i.ModalSubmitData().CustomID, ":")
		if len(parts) != 3 || parts[1] != "modal" || !validMusicSource(parts[2]) {
			musicPrivate(s, i, resources.Get("music.ui_expired"))
			return
		}
		if !musicDefer(s, i, false) {
			return
		}
		b.searchMusicUI(s, i, musicModalQuery(i.ModalSubmitData()), parts[2])
	case discordgo.InteractionMessageComponent:
		b.handleMusicComponent(s, i)
	}
}

func (b *Bot) searchMusicUI(s *discordgo.Session, i *discordgo.Interaction, query, source string) {
	b.musicUI.mu.Lock()
	if b.musicUI.searchSlots == nil {
		b.musicUI.searchSlots = make(chan struct{}, 4)
	}
	slots := b.musicUI.searchSlots
	b.musicUI.mu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.ui_busy")})
		return
	}

	query = strings.TrimSpace(query)
	identifierQuery := query
	if !strings.Contains(query, "://") {
		identifierQuery = source + ":" + query
	}
	identifier, err := musicIdentifier(identifierQuery)
	if err != nil {
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.query")})
		return
	}
	m, err := b.getMusic(s)
	if err != nil {
		log.Printf("[music] browser unavailable: %v", err)
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.unavailable")})
		return
	}
	ctx, cancel := context.WithTimeout(b.currentContext(), 20*time.Second)
	defer cancel()
	tracks, err := m.backend.load(ctx, identifier)
	if err != nil {
		log.Printf("[music] browser search failed: %v", err)
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.failed")})
		return
	}
	browser := &musicBrowser{id: musicUIID(), owner: musicActor(i).ID, guild: i.GuildID, channel: i.ChannelID, source: source, query: query, tracks: tracks[:min(len(tracks), 50)], selected: make(map[int]bool), expires: time.Now().Add(10 * time.Minute)}
	b.musicUI.mu.Lock()
	if b.musicUI.browsers == nil {
		b.musicUI.browsers = make(map[string]*musicBrowser)
	}
	for key, v := range b.musicUI.browsers {
		if time.Now().After(v.expires) {
			delete(b.musicUI.browsers, key)
		}
	}
	if len(b.musicUI.browsers) >= 256 {
		var oldest *musicBrowser
		for _, v := range b.musicUI.browsers {
			if oldest == nil || v.expires.Before(oldest.expires) {
				oldest = v
			}
		}
		delete(b.musicUI.browsers, oldest.id)
	}
	b.musicUI.browsers[browser.id] = browser
	b.musicUI.mu.Unlock()
	browser.mu.Lock()
	defer browser.mu.Unlock()
	musicEdit(s, i, browser.render())
}

func musicSearchHome(source string) *discordgo.InteractionResponseData {
	var options []discordgo.SelectMenuOption
	for _, s := range musicSources {
		options = append(options, discordgo.SelectMenuOption{Label: s.name, Value: s.value, Default: s.value == source})
	}
	return &discordgo.InteractionResponseData{Content: resources.Get("music.ui_home"), Components: []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: "music:home:source", Options: options, MaxValues: 1}}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{CustomID: "music:home:new:" + source, Label: "Новый поиск", Style: discordgo.SuccessButton}}},
	}}
}
func (b *Bot) handleMusicComponent(s *discordgo.Session, i *discordgo.Interaction) {
	data := i.MessageComponentData()
	parts := strings.Split(data.CustomID, ":")
	if len(parts) < 3 {
		musicPrivate(s, i, resources.Get("music.ui_expired"))
		return
	}
	if parts[1] == "home" {
		if parts[2] == "new" && len(parts) == 4 && validMusicSource(parts[3]) {
			musicModal(s, i, parts[3])
			return
		}
		if parts[2] == "source" && len(data.Values) == 1 && validMusicSource(data.Values[0]) {
			musicRespond(s, i, discordgo.InteractionResponseUpdateMessage, musicSearchHome(data.Values[0]))
			return
		}
		musicPrivate(s, i, resources.Get("music.ui_expired"))
		return
	}
	if parts[1] == "panel" {
		b.handleMusicPanelButton(s, i, parts)
		return
	}
	if parts[1] != "search" || len(parts) < 4 {
		musicPrivate(s, i, resources.Get("music.ui_expired"))
		return
	}
	b.musicUI.mu.Lock()
	browser := b.musicUI.browsers[parts[2]]
	b.musicUI.mu.Unlock()
	if browser == nil || browser.owner != musicActor(i).ID || browser.guild != i.GuildID || browser.channel != i.ChannelID || time.Now().After(browser.expires) {
		musicPrivate(s, i, resources.Get("music.ui_expired"))
		return
	}
	action := parts[3]
	if action == "new" {
		musicModal(s, i, browser.source)
		return
	}
	if !musicDefer(s, i, true) {
		return
	}
	if action == "back" {
		musicEdit(s, i, musicSearchHome(browser.source))
		return
	}
	browser.mu.Lock()
	if browser.busy || browser.consumed {
		musicEdit(s, i, browser.render())
		browser.mu.Unlock()
		return
	}
	switch action {
	case "select":
		page, err := parseMusicPage(parts)
		if err != nil || page != browser.page {
			musicEdit(s, i, browser.render())
			browser.mu.Unlock()
			return
		}
		start, end := browser.pageBounds()
		chosen := map[int]bool{}
		for _, value := range data.Values {
			index, err := strconv.Atoi(value)
			if err != nil || index < start || index >= end {
				musicEdit(s, i, browser.render())
				browser.mu.Unlock()
				return
			}
			chosen[index] = true
		}
		for index := start; index < end; index++ {
			delete(browser.selected, index)
		}
		for index := range chosen {
			browser.selected[index] = true
		}
	case "page":
		page, err := parseMusicPage(parts)
		if err == nil && page >= 0 && page < (len(browser.tracks)+musicSearchPageSize-1)/musicSearchPageSize {
			browser.page = page
		}
	case "add":
		var tracks []lavalink.Track
		for index, track := range browser.tracks {
			if browser.selected[index] {
				tracks = append(tracks, track)
			}
		}
		if len(tracks) == 0 {
			musicEdit(s, i, browser.render())
			browser.mu.Unlock()
			return
		}
		browser.busy = true
		musicEdit(s, i, browser.render())
		browser.mu.Unlock()
		m, err := b.getMusic(s)
		text := resources.Get("music.unavailable")
		success := false
		if err == nil {
			ctx, cancel := context.WithTimeout(b.currentContext(), musicPlayTimeout)
			text, success = b.enqueueMusicTracks(ctx, s, musicInteractionMessage(i, "play", ""), m, tracks)
			cancel()
		}
		browser.mu.Lock()
		browser.busy = false
		browser.consumed = success
		view := browser.render()
		if !success {
			view.Content = text
		}
		musicEdit(s, i, view)
		browser.mu.Unlock()
		if success {
			if err := b.refreshMusicPanel(s, i.GuildID, i.ChannelID, true); err != nil {
				log.Printf("[music] panel publish failed: %v", err)
			}
		}
		return
	}
	musicEdit(s, i, browser.render())
	browser.mu.Unlock()
}
func parseMusicPage(parts []string) (int, error) {
	if len(parts) != 5 {
		return 0, fmt.Errorf("missing page")
	}
	return strconv.Atoi(parts[4])
}
func (v *musicBrowser) pageBounds() (int, int) {
	start := v.page * musicSearchPageSize
	return start, min(len(v.tracks), start+musicSearchPageSize)
}
func (v *musicBrowser) render() *discordgo.InteractionResponseData {
	start, end := v.pageBounds()
	embed := &discordgo.MessageEmbed{Title: "Треки по запросу «" + truncateMessage(v.query, 80) + "»", Color: 0x2ecc71}
	var body strings.Builder
	var options []discordgo.SelectMenuOption
	for index := start; index < end; index++ {
		track := v.tracks[index]
		mark := "○"
		if v.selected[index] {
			mark = "●"
		}
		fmt.Fprintf(&body, "%s **%d.** %s — %s `%s`\n", mark, index+1, musicTitle(track), musicText(track.Info.Author, 60), musicLength(track))
		options = append(options, discordgo.SelectMenuOption{Label: musicText(musicTitle(track), 100), Value: strconv.Itoa(index), Description: musicText(track.Info.Author, 65) + " · " + musicLength(track), Default: v.selected[index]})
	}
	if len(v.tracks) == 0 {
		body.WriteString(resources.Get("music.empty"))
	}
	fmt.Fprintf(&body, "\n**Выбрано: %d** · Порядок соответствует списку.", len(v.selected))
	embed.Description = body.String()
	embed.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("%s · Страница %d/%d · %d треков · Поиск действует 10 минут", musicSourceName(v.source), v.page+1, max(1, (len(v.tracks)+9)/10), len(v.tracks))}
	if start < len(v.tracks) && v.tracks[start].Info.ArtworkURL != nil {
		if url := musicURL(*v.tracks[start].Info.ArtworkURL); url != "" {
			embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: url}
		}
	}
	prefix := "music:search:" + v.id + ":"
	zero := 0
	var components []discordgo.MessageComponent
	if len(options) > 0 {
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: prefix + "select:" + strconv.Itoa(v.page), Placeholder: "Выбери треки на этой странице", MinValues: &zero, MaxValues: len(options), Options: options, Disabled: v.busy || v.consumed}}})
	}
	components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: prefix + "add", Label: "Добавить выбранные", Style: discordgo.SuccessButton, Disabled: len(v.selected) == 0 || v.busy || v.consumed},
		discordgo.Button{CustomID: prefix + "page:" + strconv.Itoa(v.page-1), Label: "◀", Style: discordgo.SecondaryButton, Disabled: v.page == 0 || v.busy || v.consumed},
		discordgo.Button{CustomID: prefix + "page:" + strconv.Itoa(v.page+1), Label: "▶", Style: discordgo.SecondaryButton, Disabled: end >= len(v.tracks) || v.busy || v.consumed},
	}}, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: prefix + "back", Label: "Назад", Style: discordgo.SecondaryButton}, discordgo.Button{CustomID: prefix + "new", Label: "Новый поиск", Style: discordgo.SuccessButton},
	}})
	content := ""
	if v.busy {
		content = resources.Get("music.ui_adding")
	}
	if v.consumed {
		content = resources.Format("music.ui_added", map[string]any{"count": len(v.selected)})
	}
	return &discordgo.InteractionResponseData{Content: content, Embeds: []*discordgo.MessageEmbed{embed}, Components: components, AllowedMentions: noMusicMentions()}
}
func musicText(text string, limit int) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n", " "), "@", "＠")
	units := 0
	boundary := 0
	for index, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			return text[:boundary] + "…"
		}
		units += width
		if units <= limit-1 {
			boundary = index + len(string(r))
		}
	}
	return text
}
func musicURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}
func musicLength(track lavalink.Track) string {
	if track.Info.IsStream {
		return "LIVE"
	}
	if track.Info.Length <= 0 || track.Info.Length > lavalink.Duration((48*time.Hour).Milliseconds()) {
		return "—"
	}
	return musicDuration(track.Info.Length)
}
func musicDuration(duration lavalink.Duration) string {
	seconds := max(0, int64(duration)/1000)
	if seconds >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func musicPanelView(g *musicGuild, nonce string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	embed := &discordgo.MessageEmbed{Title: "Ничего не играет", Description: resources.Get("music.queue_empty"), Color: 0x2ecc71, Author: &discordgo.MessageEmbedAuthor{Name: "NYANDE · ПРОИГРЫВАТЕЛЬ"}}
	stopped := g.current == nil
	if !stopped {
		track := *g.current
		embed.Title = musicTitle(track)
		if track.Info.URI != nil {
			embed.URL = musicURL(*track.Info.URI)
		}
		if track.Info.ArtworkURL != nil {
			if url := musicURL(*track.Info.ArtworkURL); url != "" {
				embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: url}
			}
		}
		position := musicPosition(g, time.Now())
		progress := ""
		if track.Info.Length > 0 && !track.Info.IsStream && musicLength(track) != "—" {
			filled := min(19, int(float64(position)/float64(track.Info.Length)*20))
			progress = "\n`" + strings.Repeat("━", filled) + "●" + strings.Repeat("─", 19-filled) + "`"
		}
		mode := g.repeat
		if mode == "" {
			mode = "off"
		}
		embed.Description = fmt.Sprintf("%s · %s\n\n`%s / %s`%s\n\nПовтор: **%s** · Громкость: **%d%%** · В очереди: **%d**", musicText(track.Info.Author, 100), musicText(track.Info.SourceName, 30), musicDuration(position), musicLength(track), progress, mode, g.volume, len(g.queue))
		if g.paused {
			embed.Description += "\n⏸ Пауза"
		}
	}
	button := func(action, label string, style discordgo.ButtonStyle) discordgo.Button {
		return discordgo.Button{CustomID: "music:panel:" + nonce + ":" + action, Label: label, Style: style, Disabled: stopped}
	}
	pause := "⏸"
	if g.paused {
		pause = "▶"
	}
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{button("toggle", pause, discordgo.SuccessButton), button("skip", "⏭", discordgo.SecondaryButton), button("repeat", "🔁", discordgo.SecondaryButton), button("shuffle", "🔀", discordgo.SecondaryButton), button("stop", "⏹", discordgo.DangerButton)}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{button("voldown", "🔉 −10", discordgo.SecondaryButton), button("volup", "🔊 +10", discordgo.SecondaryButton), button("queue", "📜 Очередь", discordgo.SecondaryButton)}},
	}
	return embed, components
}
func (b *Bot) refreshMusicPanel(s *discordgo.Session, guild, channel string, create bool) error {
	b.musicMu.Lock()
	m := b.music
	b.musicMu.Unlock()
	if m == nil {
		return nil
	}
	b.musicUI.panelMu.Lock()
	defer b.musicUI.panelMu.Unlock()
	if b.musicUI.panels == nil {
		b.musicUI.panels = make(map[string]musicPanel)
	}
	panel, exists := b.musicUI.panels[guild]
	if !exists && !create {
		return nil
	}
	g := m.guild(guild)
	g.mu.Lock()
	if !exists && g.current == nil {
		g.mu.Unlock()
		return nil
	}
	if !exists {
		panel = musicPanel{channel: channel, nonce: musicUIID()}
	}
	embed, components := musicPanelView(g, panel.nonce)
	stopped := g.current == nil
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(b.currentContext(), 5*time.Second)
	defer cancel()
	if exists {
		_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: panel.id, Channel: panel.channel, Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components, AllowedMentions: noMusicMentions()}, discordgo.WithContext(ctx))
		if err != nil {
			var api *discordgo.RESTError
			if errors.As(err, &api) && api.Message != nil && (api.Message.Code == 10008 || api.Message.Code == 10003 || api.Message.Code == 50001 || api.Message.Code == 50013) {
				delete(b.musicUI.panels, guild)
			}
			return err
		}
	} else {
		message, err := s.ChannelMessageSendComplex(panel.channel, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: components, AllowedMentions: noMusicMentions()}, discordgo.WithContext(ctx))
		if err != nil {
			return err
		}
		panel.id = message.ID
		b.musicUI.panels[guild] = panel
	}
	if stopped {
		delete(b.musicUI.panels, guild)
	}
	return nil
}
func (b *Bot) handleMusicPanelButton(s *discordgo.Session, i *discordgo.Interaction, parts []string) {
	if len(parts) != 4 || i.Message == nil {
		musicPrivate(s, i, resources.Get("music.ui_expired"))
		return
	}
	if !musicDefer(s, i, false) {
		return
	}
	b.musicUI.panelMu.Lock()
	panel, exists := b.musicUI.panels[i.GuildID]
	b.musicUI.panelMu.Unlock()
	if !exists || panel.nonce != parts[2] || panel.id != i.Message.ID || panel.channel != i.ChannelID {
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.ui_expired")})
		return
	}
	action := parts[3]
	allowed := map[string]bool{"toggle": true, "skip": true, "repeat": true, "shuffle": true, "stop": true, "voldown": true, "volup": true, "queue": true}
	if !allowed[action] {
		musicEdit(s, i, &discordgo.InteractionResponseData{Content: resources.Get("music.ui_expired")})
		return
	}
	text := b.musicCommand(s, musicInteractionMessage(i, action, ""), action)
	musicEdit(s, i, &discordgo.InteractionResponseData{Content: text})
	if action != "queue" {
		if err := b.refreshMusicPanel(s, i.GuildID, i.ChannelID, false); err != nil {
			log.Printf("[music] panel refresh failed: %v", err)
		}
	}
}
func (b *Bot) musicPanelLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.musicUI.panelMu.Lock()
			guilds := make([]string, 0, len(b.musicUI.panels))
			for guild := range b.musicUI.panels {
				guilds = append(guilds, guild)
			}
			b.musicUI.panelMu.Unlock()
			for _, guild := range guilds {
				if ctx.Err() != nil {
					return
				}
				if err := b.refreshMusicPanel(b.session, guild, "", false); err != nil {
					log.Printf("[music] periodic panel update failed: %v", err)
				}
			}
			b.musicUI.mu.Lock()
			for key, v := range b.musicUI.browsers {
				if time.Now().After(v.expires) {
					delete(b.musicUI.browsers, key)
				}
			}
			b.musicUI.mu.Unlock()
		}
	}
}
