package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/schedules"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

func scheduleOwner(m *telegram.Message) bool {
	return m != nil && m.IsPrivate() && m.From != nil && !m.From.IsBot && m.From.ID > 0 && m.Chat.ID == m.From.ID
}

func (b *Bot) scheduleReply(ctx context.Context, m *telegram.Message, text string, markup any) error {
	parts := splitTelegramMessage(text, 4096)
	for i, part := range parts {
		var keyboard any
		if i == len(parts)-1 {
			keyboard = markup
		}
		if _, err := b.telegram.SendMessage(ctx, m.Chat.ID, part, 0, keyboard); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) scheduleCommand(ctx context.Context, m *telegram.Message, args string) error {
	if !scheduleOwner(m) {
		return b.scheduleReply(ctx, m, resources.Get("schedule.private"), nil)
	}
	if b.schedules == nil {
		return b.scheduleReply(ctx, m, resources.Get("schedule.unavailable"), nil)
	}
	fields := strings.Fields(args)
	command := ""
	if len(fields) > 0 {
		command = strings.ToLower(fields[0])
	}
	switch command {
	case "cancel":
		if err := b.schedules.DeleteDraft(ctx, m.From.ID, m.Chat.ID); err != nil {
			return err
		}
		return b.scheduleReply(ctx, m, resources.Get("schedule.cancelled"), nil)
	case "off":
		if err := b.schedules.Disable(ctx, m.From.ID, m.Chat.ID); err != nil {
			return err
		}
		if err := b.schedules.DeleteDraft(ctx, m.From.ID, m.Chat.ID); err != nil {
			return err
		}
		return b.scheduleReply(ctx, m, resources.Get("schedule.disabled"), nil)
	case "skip":
		if len(fields) != 3 {
			return b.scheduleReply(ctx, m, resources.Get("schedule.skip_usage"), nil)
		}
		n, err := b.schedules.Skip(ctx, m.From.ID, m.Chat.ID, fields[1], fields[2])
		if err != nil {
			return b.scheduleReply(ctx, m, resources.Get("schedule.skip_usage"), nil)
		}
		return b.scheduleReply(ctx, m, resources.Format("schedule.skipped", map[string]any{"count": n}), nil)
	case "new", "edit":
		if !b.cfg.LLMEnabled || b.llm == nil {
			return b.scheduleReply(ctx, m, resources.Get("schedule.llm_disabled"), nil)
		}
		if command == "edit" {
			active, err := b.schedules.GetActive(ctx, m.From.ID, m.Chat.ID)
			if errors.Is(err, sql.ErrNoRows) {
				return b.scheduleReply(ctx, m, resources.Get("schedule.no_active"), nil)
			}
			if err != nil {
				return err
			}
			d, err := b.schedules.SaveDraft(ctx, schedules.Draft{UserID: m.From.ID, ChatID: m.Chat.ID, Plan: active.Plan, Stage: "review"})
			if err != nil {
				return err
			}
			return b.showScheduleDraft(ctx, m, d)
		}
		d, err := b.newScheduleDraft(ctx, m)
		if err != nil {
			return err
		}
		return b.showScheduleDraft(ctx, m, d)
	case "", "show":
		d, err := b.schedules.GetDraft(ctx, m.From.ID, m.Chat.ID)
		if err == nil {
			return b.showScheduleDraft(ctx, m, d)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		active, err := b.schedules.GetActive(ctx, m.From.ID, m.Chat.ID)
		if err == nil {
			return b.scheduleReply(ctx, m, resources.Get("schedule.active")+"\n"+schedulePreview(active.Plan)+"\n\n"+resources.Get("schedule.manage"), nil)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if command == "show" {
			return b.scheduleReply(ctx, m, resources.Get("schedule.no_active"), nil)
		}
		return b.scheduleCommand(ctx, m, "new")
	default:
		return b.scheduleReply(ctx, m, resources.Get("schedule.manage"), nil)
	}
}

func (b *Bot) newScheduleDraft(ctx context.Context, m *telegram.Message) (schedules.Draft, error) {
	zone := strings.TrimSpace(b.cfg.LLMTimezone)
	if _, err := time.LoadLocation(zone); err != nil || zone == "" || zone == "Local" {
		zone = "Asia/Almaty"
	}
	return b.schedules.SaveDraft(ctx, schedules.Draft{UserID: m.From.ID, ChatID: m.Chat.ID, Stage: "collect", Plan: schedules.Plan{Timezone: zone, LeadMinutes: 10}})
}

// Collect all album parts before invoking vision. The user chooses when the
// batch is complete; no timer guesses whether another screenshot will arrive.
func (b *Bot) handleScheduleInput(ctx context.Context, m *telegram.Message) (bool, error) {
	if b.schedules == nil || !scheduleOwner(m) {
		return false, nil
	}
	d, err := b.schedules.GetDraft(ctx, m.From.ID, m.Chat.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	input, ok := scheduleInput(m)
	if !ok {
		return true, b.scheduleReply(ctx, m, resources.Get("schedule.input_hint"), nil)
	}
	wasReview := d.Stage == "review"
	for _, existing := range d.Inputs {
		if existing.MessageID == input.MessageID || (input.FileID != "" && existing.FileID == input.FileID) {
			return true, nil
		}
	}
	// Plain text is retained as notes, without counting against the screenshot cap.
	if input.Text != "" {
		if !scheduleNotesFit(d.Notes, input.Text) {
			return true, b.scheduleReply(ctx, m, resources.Get("schedule.notes_limit"), nil)
		}
		d.Notes = append(d.Notes, input.Text)
	}
	if input.FileID != "" {
		if !b.cfg.LLMVision {
			return true, b.scheduleReply(ctx, m, resources.Get("schedule.vision_disabled"), nil)
		}
		if len(d.Inputs) >= 12 {
			return true, b.scheduleReply(ctx, m, resources.Get("schedule.images_limit"), nil)
		}
		d.Inputs = append(d.Inputs, input)
	}
	// Pending corrections invalidate confirmation until extraction succeeds.
	d.Stage = "collect"
	d, err = b.schedules.SaveDraft(ctx, d)
	if err != nil {
		return true, err
	}
	if wasReview && input.FileID == "" {
		return true, b.recognizeSchedule(ctx, m, d)
	}
	return true, b.updateScheduleCollection(ctx, d)
}

func (b *Bot) updateScheduleCollection(ctx context.Context, d schedules.Draft) error {
	text := resources.Format("schedule.collected", map[string]any{"count": len(d.Inputs)})
	markup := scheduleCollectKeyboard(d.Revision)
	if d.CollectionMessageID != 0 {
		if err := b.telegram.EditMessageText(ctx, d.ChatID, d.CollectionMessageID, text, markup); err == nil {
			return nil
		}
	}
	// Keep one counter for the whole batch, including album parts arriving in
	// separate updates. Recreate the status if its message has been deleted.
	message, err := b.telegram.SendMessage(ctx, d.ChatID, text, 0, markup)
	if err != nil {
		return err
	}
	d.CollectionMessageID = message.MessageID
	_, err = b.schedules.SaveDraft(ctx, d)
	return err
}

func scheduleNotesFit(notes []string, next string) bool {
	if len(notes) >= 32 {
		return false
	}
	all := append(append([]string(nil), notes...), next)
	encoded, err := json.Marshal(all)
	return err == nil && len(encoded) <= 16*1024
}

func scheduleInput(m *telegram.Message) (schedules.Input, bool) {
	in := schedules.Input{MessageID: m.MessageID, GroupID: m.MediaGroupID, Text: strings.TrimSpace(m.ContentText())}
	if len(m.Photo) > 0 {
		in.FileID, in.MIME = m.Photo[len(m.Photo)-1].FileID, "image/jpeg"
	} else if m.Document != nil {
		switch m.Document.MimeType {
		case "image/jpeg", "image/png", "image/webp":
			in.FileID, in.MIME = m.Document.FileID, m.Document.MimeType
		default:
			return in, false
		}
	}
	return in, in.FileID != "" || in.Text != ""
}

func scheduleCollectKeyboard(revision int64) any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": resources.Get("schedule.button.recognize"), "callback_data": fmt.Sprintf("sch:recognize:%d", revision)},
	}}}
}

func (b *Bot) showScheduleDraft(ctx context.Context, m *telegram.Message, d schedules.Draft) error {
	if d.Stage != "review" {
		return b.scheduleReply(ctx, m, resources.Format("schedule.intro", map[string]any{"zone": d.Plan.Timezone, "count": len(d.Inputs)}), scheduleCollectKeyboard(d.Revision))
	}
	text := resources.Get("schedule.preview") + "\n" + schedulePreview(d.Plan)
	issues := schedules.Issues(d.Plan)
	if len(d.Plan.Questions) > 0 {
		issues = d.Plan.Questions
	}
	rows := [][]map[string]string{}
	if len(issues) > 0 {
		text += "\n\n" + resources.Get("schedule.clarify") + "\n• " + strings.Join(issues, "\n• ")
	} else {
		occurrences, err := schedules.Generate(d.Plan, time.Now())
		if err == nil && len(occurrences) > 0 {
			text += "\n\n" + resources.Get("schedule.upcoming")
			loc, _ := time.LoadLocation(d.Plan.Timezone)
			for _, occurrence := range occurrences[:min(3, len(occurrences))] {
				text += "\n" + occurrence.StartAt.In(loc).Format("02.01.2006 15:04") + " — " + occurrence.Lesson.Subject
			}
		}
		text += "\n\n" + resources.Get("schedule.confirm")
		row := []map[string]string{}
		for _, lead := range []int{10, 30, 60} {
			row = append(row, map[string]string{"text": resources.Format("schedule.button.activate", map[string]any{"minutes": lead}), "callback_data": fmt.Sprintf("sch:activate:%d:%d", d.Revision, lead)})
		}
		rows = append(rows, row)
	}
	text += "\n\n" + resources.Get("schedule.correct")
	rows = append(rows, []map[string]string{{"text": resources.Get("schedule.button.cancel"), "callback_data": fmt.Sprintf("sch:cancel:%d", d.Revision)}})
	return b.scheduleReply(ctx, m, text, map[string]any{"inline_keyboard": rows})
}

func schedulePreview(p schedules.Plan) string {
	unknown := resources.Get("schedule.unknown")
	value := func(s string) string {
		if s == "" {
			return unknown
		}
		return s
	}
	var text strings.Builder
	text.WriteString(resources.Format("schedule.metadata", map[string]any{"zone": value(p.Timezone), "start": value(p.StartDate), "end": value(p.EndDate), "cycle": p.CycleWeeks, "anchor": value(p.AnchorDate), "lead": p.LeadMinutes}))
	weekdays := strings.Split(resources.Get("schedule.weekdays"), "|")
	for _, l := range p.Lessons {
		day := l.Date
		if day == "" {
			day = unknown
			if l.Weekday >= 1 && l.Weekday <= 7 {
				day = weekdays[l.Weekday-1]
			}
			if l.Week == 0 {
				day += " · " + resources.Get("schedule.every_week")
			} else {
				day += " · " + resources.Format("schedule.week", map[string]any{"week": l.Week})
			}
		}
		location := value(l.Room)
		if l.Building != "" {
			location += ", " + l.Building
		}
		text.WriteString("\n\n" + day + " · " + value(l.Start))
		if l.End != "" {
			text.WriteString("–" + l.End)
		}
		text.WriteString("\n" + value(l.Subject) + "\n" + resources.Format("schedule.room", map[string]any{"room": location}))
		if l.Kind != "" {
			text.WriteString(" · " + l.Kind)
		}
	}
	return text.String()
}

func (b *Bot) recognizeSchedule(ctx context.Context, m *telegram.Message, d schedules.Draft) error {
	if b.llm == nil || !b.cfg.LLMEnabled {
		return b.scheduleReply(ctx, m, resources.Get("schedule.llm_disabled"), nil)
	}
	if len(d.Inputs) == 0 && len(d.Notes) == 0 {
		return b.showScheduleDraft(ctx, m, d)
	}
	responseCtx := ctx
	status := resources.Get("schedule.recognizing")
	if len(d.Inputs) > 0 {
		status = resources.Format("schedule.recognizing_images", map[string]any{"count": len(d.Inputs)})
	}
	if err := b.scheduleReply(ctx, m, status, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stopTyping := make(chan struct{})
	go b.typingLoop(ctx, m.Chat.ID, stopTyping)
	defer close(stopTyping)
	images := make([]llm.Image, 0, len(d.Inputs))
	total := 0
	for _, input := range d.Inputs {
		if input.FileID == "" {
			continue
		}
		data, _, err := b.telegram.DownloadFile(ctx, input.FileID, 10<<20)
		if err != nil {
			return b.scheduleReply(responseCtx, m, resources.Get("schedule.download_failed"), scheduleCollectKeyboard(d.Revision))
		}
		total += len(data)
		if total > 20<<20 {
			return b.scheduleReply(ctx, m, resources.Get("schedule.images_limit"), nil)
		}
		images = append(images, llm.Image{MIME: http.DetectContentType(data), Data: data})
	}
	plan, err := b.llm.ExtractSchedule(ctx, d.Plan, d.Notes, images)
	if err != nil {
		// Provider errors may contain echoed source data; do not log the timetable.
		log.Printf(resources.Get("schedule.log.extract_failed"), m.Chat.ID)
		return b.scheduleReply(responseCtx, m, resources.Get("schedule.recognition_failed"), scheduleCollectKeyboard(d.Revision))
	}
	d.Plan, d.Stage = plan, "review"
	// The revised plan now contains these inputs. Replaying relative edits or
	// earlier screenshots in later requests could undo corrections or apply twice.
	d.Inputs, d.Notes = nil, nil
	d.CollectionMessageID = 0
	d, err = b.schedules.SaveDraft(responseCtx, d)
	if errors.Is(err, schedules.ErrDraftChanged) {
		return b.scheduleReply(responseCtx, m, resources.Get("schedule.stale"), nil)
	}
	if err != nil {
		return err
	}
	return b.showScheduleDraft(responseCtx, m, d)
}

func (b *Bot) handleScheduleCallback(ctx context.Context, q *telegram.CallbackQuery) error {
	if q.Message == nil || !q.Message.IsPrivate() || q.From.IsBot || q.From.ID <= 0 || q.Message.Chat.ID != q.From.ID || b.schedules == nil {
		return b.telegram.AnswerCallback(ctx, q.ID, resources.Get("schedule.stale"), "", false)
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) < 3 {
		return b.telegram.AnswerCallback(ctx, q.ID, resources.Get("schedule.stale"), "", false)
	}
	revision, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return b.telegram.AnswerCallback(ctx, q.ID, resources.Get("schedule.stale"), "", false)
	}
	d, err := b.schedules.GetDraft(ctx, q.From.ID, q.Message.Chat.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Album collection revises the draft after its first button was sent.
	// Recognition may use the latest batch; activation always requires exact CAS.
	if err != nil || (revision != d.Revision && parts[1] != "recognize") {
		return b.telegram.AnswerCallback(ctx, q.ID, resources.Get("schedule.stale"), "", false)
	}
	if err := b.telegram.AnswerCallback(ctx, q.ID, "", "", false); err != nil {
		return err
	}
	m := *q.Message
	m.From = &q.From
	switch parts[1] {
	case "recognize":
		return b.recognizeSchedule(ctx, &m, d)
	case "cancel":
		return b.scheduleCommand(ctx, &m, "cancel")
	case "activate":
		if len(parts) != 4 || d.Stage != "review" {
			return b.scheduleReply(ctx, &m, resources.Get("schedule.stale"), nil)
		}
		lead, err := strconv.Atoi(parts[3])
		if err != nil || (lead != 10 && lead != 30 && lead != 60) {
			return b.scheduleReply(ctx, &m, resources.Get("schedule.stale"), nil)
		}
		plan := d.Plan
		plan.LeadMinutes = lead
		upcoming, generateErr := schedules.Generate(plan, time.Now())
		if generateErr == nil && len(upcoming) == 0 {
			return b.scheduleReply(ctx, &m, resources.Get("schedule.no_future"), nil)
		}
		active, count, err := b.schedules.Activate(ctx, q.From.ID, m.Chat.ID, revision, lead, time.Now())
		if err != nil {
			if errors.Is(err, schedules.ErrDraftChanged) {
				return b.scheduleReply(ctx, &m, resources.Get("schedule.stale"), nil)
			}
			return b.scheduleReply(ctx, &m, resources.Format("schedule.activation_failed", map[string]any{"error": err.Error()}), nil)
		}
		return b.scheduleReply(ctx, &m, resources.Format("schedule.activated", map[string]any{"count": count, "minutes": lead, "zone": active.Plan.Timezone})+"\n\n"+resources.Get("schedule.manage"), nil)
	default:
		return nil
	}
}

func (b *Bot) scheduleTools(m *telegram.Message) []llm.Tool {
	if b.schedules == nil || !scheduleOwner(m) {
		return nil
	}
	return []llm.Tool{{
		Name: "start_class_schedule", Description: resources.Get("schedule.tool.description"),
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		Execute: func(ctx context.Context, _ map[string]string) (string, error) {
			d, err := b.schedules.GetDraft(ctx, m.From.ID, m.Chat.ID)
			if errors.Is(err, sql.ErrNoRows) {
				d, err = b.newScheduleDraft(ctx, m)
			}
			if err != nil {
				return "", err
			}
			if _, err = b.handleScheduleInput(ctx, m); err != nil {
				return "", err
			}
			if len(d.Inputs) == 0 && len(d.Notes) == 0 && m.ContentText() == "" && len(m.Photo) == 0 {
				_ = b.showScheduleDraft(ctx, m, d)
			}
			return resources.Get("schedule.tool.result"), nil
		},
	}}
}

func (b *Bot) scheduleLoop(ctx context.Context) {
	if b.schedules == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			due, err := b.schedules.TakeDue(ctx, time.Now(), 20)
			if err != nil {
				log.Printf(resources.Get("schedule.log.due_failed"), err)
				continue
			}
			var workers sync.WaitGroup
			capacity := make(chan struct{}, 8)
			for _, notification := range due {
				capacity <- struct{}{}
				workers.Add(1)
				go func(n schedules.Notification) {
					defer workers.Done()
					defer func() { <-capacity }()
					b.deliverSchedule(ctx, n)
				}(notification)
			}
			workers.Wait()
		}
	}
}

func (b *Bot) deliverSchedule(ctx context.Context, n schedules.Notification) {
	if n.ChatID != n.UserID || n.UserID <= 0 || !n.StartAt.After(time.Now()) {
		return
	}
	if b.access != nil && b.access.IsBanned(strconv.FormatInt(n.UserID, 10)) {
		return
	}
	if b.schedules == nil {
		return
	}
	allowed, err := b.schedules.CanDeliver(ctx, n)
	if err != nil {
		log.Printf(resources.Get("schedule.log.due_failed"), err)
		return
	}
	if !allowed {
		return
	}
	// This sender does not wait for ongoing LLM conversations in the same chat.
	// TakeDue has already consumed the occurrence: an ambiguous send is not retried.
	deadline := minTime(n.StartAt, time.Now().Add(20*time.Second))
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, err := b.telegram.SendMessage(ctx, n.ChatID, scheduleNotificationText(n), 0, nil); err != nil {
		log.Printf(resources.Get("schedule.log.send_failed"), n.ID)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func scheduleNotificationText(n schedules.Notification) string {
	zone, err := time.LoadLocation(n.Timezone)
	if err != nil {
		zone = time.UTC
	}
	room := n.Lesson.Room
	if room == "" {
		room = resources.Get("schedule.unknown")
	}
	if n.Lesson.Building != "" {
		room += ", " + n.Lesson.Building
	}
	return resources.Format("schedule.notification", map[string]any{"subject": n.Lesson.Subject, "date": n.StartAt.In(zone).Format("02.01.2006"), "start": n.StartAt.In(zone).Format("15:04"), "room": room, "zone": n.Timezone})
}
