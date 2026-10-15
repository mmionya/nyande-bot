package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mmionya/nyande-bot/internal/llm"
	"github.com/mmionya/nyande-bot/internal/schedules"
	"github.com/mmionya/nyande-bot/internal/telegram"
	"github.com/mmionya/nyande-bot/resources"
)

type scheduleSentMessage struct {
	Text        string `json:"text"`
	ReplyMarkup struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	} `json:"reply_markup"`
}

type scheduleLLMRequest struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type scheduleTestNetwork struct {
	mu       sync.Mutex
	messages []scheduleSentMessage
	answers  []string
	files    []string
	requests []scheduleLLMRequest
	plan     schedules.Plan
	typed    chan struct{}
}

func scheduleUITestPlan(t *testing.T) schedules.Plan {
	t.Helper()
	zone, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatal(err)
	}
	return schedules.Plan{
		Timezone: "Asia/Almaty", LeadMinutes: 10, Questions: []string{},
		Lessons: []schedules.Lesson{{Date: time.Now().In(zone).AddDate(0, 0, 2).Format("2006-01-02"), Start: "11:00", End: "12:15", Subject: "Основы менеджмента", Room: "304", Building: "Главный учебный корпус"}},
	}
}

func scheduleUITestBot(t *testing.T, plan schedules.Plan) (*Bot, *scheduleTestNetwork) {
	t.Helper()
	b := concurrentTestBot()
	b.cfg.LLMVision, b.cfg.LLMTimezone = true, "Asia/Almaty"
	b.llm = llm.New(b.cfg)
	s, err := schedules.Open(filepath.Join(t.TempDir(), "schedules.db"))
	if err != nil {
		t.Fatal(err)
	}
	b.schedules = s
	t.Cleanup(func() { _ = s.Close() })
	network := &scheduleTestNetwork{plan: plan, typed: make(chan struct{}, 8)}
	previous := http.DefaultTransport
	http.DefaultTransport = moderationTransport(func(r *http.Request) (*http.Response, error) {
		body, contentType := `{"ok":true,"result":{"message_id":100}}`, "application/json"
		switch {
		case r.URL.Hostname() == "llm.test":
			var payload scheduleLLMRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			// Recognition starts a typing goroutine. Synchronize its first request
			// before returning, so restoring DefaultTransport cannot leak a request.
			select {
			case <-network.typed:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			network.mu.Lock()
			network.requests = append(network.requests, payload)
			data, err := json.Marshal(network.plan)
			network.mu.Unlock()
			if err != nil {
				t.Error(err)
			}
			response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(data)}}}})
			if err != nil {
				t.Error(err)
			}
			body = string(response)
		case r.URL.Hostname() != "api.telegram.org":
			t.Errorf("unexpected network host: %s", r.URL.Hostname())
		case strings.Contains(r.URL.Path, "/file/bottest/"):
			body, contentType = string([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0}), "image/jpeg"
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			var payload struct {
				FileID string `json:"file_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			network.mu.Lock()
			network.files = append(network.files, payload.FileID)
			network.mu.Unlock()
			data, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"file_id": payload.FileID, "file_path": "photos/" + payload.FileID + ".jpg", "file_size": 8}})
			if err != nil {
				t.Error(err)
			}
			body = string(data)
		case strings.HasSuffix(r.URL.Path, "/sendChatAction"):
			network.typed <- struct{}{}
			body = `{"ok":true,"result":true}`
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var payload scheduleSentMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			network.mu.Lock()
			network.messages = append(network.messages, payload)
			network.mu.Unlock()
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			network.mu.Lock()
			network.answers = append(network.answers, payload.Text)
			network.mu.Unlock()
			body = `{"ok":true,"result":true}`
		default:
			t.Errorf("unexpected Telegram method: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return b, network
}

func scheduleUIMessage(id int, text string) *telegram.Message {
	return &telegram.Message{MessageID: id, Chat: telegram.Chat{ID: 1, Type: "private"}, From: &telegram.User{ID: 1}, Text: text}
}

func scheduleUIUpdate(t *testing.T, b *Bot, update telegram.Update) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.HandleUpdate(ctx, update); err != nil {
		t.Fatal(err)
	}
}

func scheduleUICallback(data string) *telegram.CallbackQuery {
	return &telegram.CallbackQuery{ID: "test-callback", From: telegram.User{ID: 1}, Message: scheduleUIMessage(100, ""), Data: data}
}

func TestScheduleAlbumCollectsAllImagesBeforeRecognition(t *testing.T) {
	b, network := scheduleUITestBot(t, scheduleUITestPlan(t))
	scheduleUIUpdate(t, b, telegram.Update{Message: scheduleUIMessage(1, "/schedule new")})
	var firstButton string
	for i := 0; i < 3; i++ {
		m := scheduleUIMessage(2+i, "")
		m.MediaGroupID = "album-1"
		m.Photo = []telegram.PhotoSize{{FileID: fmt.Sprintf("thumb-%d", i)}, {FileID: fmt.Sprintf("full-%d", i)}}
		if i == 0 {
			m.Caption = "Моё расписание"
		}
		scheduleUIUpdate(t, b, telegram.Update{Message: m})
		if i == 0 {
			last := network.messages[len(network.messages)-1]
			firstButton = last.ReplyMarkup.InlineKeyboard[0][0].CallbackData
		}
		// Both a duplicated update and a new message carrying the same file must
		// leave the draft unchanged and create no additional recognition calls.
		scheduleUIUpdate(t, b, telegram.Update{Message: m})
		copy := *m
		copy.MessageID += 1000
		scheduleUIUpdate(t, b, telegram.Update{Message: &copy})
	}
	draft, err := b.schedules.GetDraft(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Inputs) != 3 || len(draft.Notes) != 1 || len(network.requests) != 0 {
		t.Fatalf("album collection: inputs=%d notes=%d LLM requests=%d", len(draft.Inputs), len(draft.Notes), len(network.requests))
	}
	if len(network.messages) != 2 {
		t.Fatalf("album acknowledgement spam: %d messages", len(network.messages))
	}
	if firstButton == fmt.Sprintf("sch:recognize:%d", draft.Revision) {
		t.Fatal("fixture did not produce an older album button")
	}
	scheduleUIUpdate(t, b, telegram.Update{CallbackQuery: scheduleUICallback(firstButton)})
	if len(network.requests) != 1 || strings.Join(network.files, ",") != "full-0,full-1,full-2" {
		t.Fatalf("first album button failed to recognize latest complete batch: requests=%d files=%v", len(network.requests), network.files)
	}
	var content []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(network.requests[0].Messages[1].Content, &content); err != nil {
		t.Fatal(err)
	}
	images := 0
	for _, part := range content {
		if part.Type == "image_url" {
			images++
		}
	}
	if images != 3 {
		t.Fatalf("vision request received %d images", images)
	}
	draft, err = b.schedules.GetDraft(context.Background(), 1, 1)
	if err != nil || draft.Stage != "review" || len(draft.Plan.Lessons) != 1 {
		t.Fatalf("recognition did not create review draft: %+v, %v", draft, err)
	}
	if len(draft.Inputs) != 0 || len(draft.Notes) != 0 {
		t.Fatal("recognized images and notes were retained for replay")
	}
	if _, err := b.schedules.GetActive(context.Background(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("recognition activated without confirmation: %v", err)
	}
}

func TestScheduleActivationRequiresOwnerPrivateChatAndCurrentRevision(t *testing.T) {
	b, network := scheduleUITestBot(t, scheduleUITestPlan(t))
	draft, err := b.schedules.SaveDraft(context.Background(), schedules.Draft{UserID: 1, ChatID: 1, Plan: scheduleUITestPlan(t), Stage: "review"})
	if err != nil {
		t.Fatal(err)
	}
	oldRevision := draft.Revision
	draft, err = b.schedules.SaveDraft(context.Background(), draft)
	if err != nil {
		t.Fatal(err)
	}
	validData := fmt.Sprintf("sch:activate:%d:30", draft.Revision)
	cases := []*telegram.CallbackQuery{
		scheduleUICallback(fmt.Sprintf("sch:activate:%d:30", oldRevision)),
		scheduleUICallback(validData),
		scheduleUICallback(validData),
		scheduleUICallback(validData),
	}
	cases[1].From.ID = 2
	cases[2].Message.Chat.Type = "group"
	cases[3].From.IsBot = true
	for _, callback := range cases {
		scheduleUIUpdate(t, b, telegram.Update{CallbackQuery: callback})
		if _, err := b.schedules.GetActive(context.Background(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unauthorized or stale callback activated: %+v, %v", callback, err)
		}
		if last := network.answers[len(network.answers)-1]; last != resources.Get("schedule.stale") {
			t.Fatalf("invalid callback answer: %q", last)
		}
	}
	scheduleUIUpdate(t, b, telegram.Update{CallbackQuery: scheduleUICallback(validData)})
	active, err := b.schedules.GetActive(context.Background(), 1, 1)
	if err != nil || active.Revision != draft.Revision || active.Plan.LeadMinutes != 30 {
		t.Fatalf("valid confirmation failed: %+v, %v", active, err)
	}
	if _, err := b.schedules.GetDraft(context.Background(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("confirmed draft remains: %v", err)
	}
	if !strings.Contains(network.messages[len(network.messages)-1].Text, "30") {
		t.Fatal("confirmation omitted selected lead")
	}
	scheduleUIUpdate(t, b, telegram.Update{CallbackQuery: scheduleUICallback(validData)})
	if last := network.answers[len(network.answers)-1]; last != resources.Get("schedule.stale") {
		t.Fatalf("replayed activation was accepted: %q", last)
	}
}

func TestScheduleReviewTextCorrectionCallsExtractorWithoutActivation(t *testing.T) {
	plan := scheduleUITestPlan(t)
	corrected := scheduleUITestPlan(t)
	corrected.Lessons[0].Room = "425"
	b, network := scheduleUITestBot(t, corrected)
	draft, err := b.schedules.SaveDraft(context.Background(), schedules.Draft{UserID: 1, ChatID: 1, Plan: plan, Stage: "review"})
	if err != nil {
		t.Fatal(err)
	}
	correction := "Кабинет у менеджмента исправь на 425"
	scheduleUIUpdate(t, b, telegram.Update{Message: scheduleUIMessage(10, correction)})
	if len(network.requests) != 1 {
		t.Fatalf("correction made %d LLM requests", len(network.requests))
	}
	var content string
	if err := json.Unmarshal(network.requests[0].Messages[1].Content, &content); err != nil {
		t.Fatal(err)
	}
	var source struct {
		CurrentPlan schedules.Plan `json:"current_plan"`
		Notes       []string       `json:"notes"`
	}
	if err := json.Unmarshal([]byte(content), &source); err != nil {
		t.Fatal(err)
	}
	if len(source.Notes) != 1 || source.Notes[0] != correction || source.CurrentPlan.Lessons[0].Room != "304" {
		t.Fatalf("extractor missed current plan or correction: %+v", source)
	}
	revised, err := b.schedules.GetDraft(context.Background(), 1, 1)
	if err != nil || revised.Stage != "review" || revised.Revision <= draft.Revision || revised.Plan.Lessons[0].Room != "425" {
		t.Fatalf("corrected review not saved: %+v, %v", revised, err)
	}
	if _, err := b.schedules.GetActive(context.Background(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("correction activated without confirmation: %v", err)
	}
	last := network.messages[len(network.messages)-1]
	if !strings.Contains(last.Text, "425") || len(last.ReplyMarkup.InlineKeyboard) != 2 {
		t.Fatalf("correction review lacks room or activation choices: %+v", last)
	}
}

func TestScheduleCommandIsPrivateOnly(t *testing.T) {
	b, network := scheduleUITestBot(t, scheduleUITestPlan(t))
	m := scheduleUIMessage(1, "/schedule new")
	m.Chat = telegram.Chat{ID: -100, Type: "supergroup"}
	scheduleUIUpdate(t, b, telegram.Update{Message: m})
	if len(network.messages) != 1 || network.messages[0].Text != resources.Get("schedule.private") {
		t.Fatalf("group command response: %+v", network.messages)
	}
	if _, err := b.schedules.GetDraft(context.Background(), 1, -100); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("group schedule draft created: %v", err)
	}
}

func TestScheduleNotificationTextUsesClassTimezoneAndRoom(t *testing.T) {
	n := schedules.Notification{StartAt: time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC), Timezone: "Asia/Almaty", LeadMinutes: 30, Lesson: schedules.Lesson{Subject: "Основы менеджмента", Room: "304", Building: "Главный учебный корпус"}}
	text := scheduleNotificationText(n)
	for _, want := range []string{"Основы менеджмента", "15.10.2026", "11:00", "Asia/Almaty", "304, Главный учебный корпус"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification missing %q: %s", want, text)
		}
	}
	n.Lesson.Room, n.Lesson.Building = "", ""
	if text := scheduleNotificationText(n); !strings.Contains(text, resources.Get("schedule.unknown")) {
		t.Fatalf("missing room was not marked unknown: %s", text)
	}
}

func TestScheduleCorrectionsDoNotReplayConsumedRelativeEdits(t *testing.T) {
	plan := scheduleUITestPlan(t)
	shifted := scheduleUITestPlan(t)
	shifted.Lessons[0].Start, shifted.Lessons[0].End = "12:00", "13:15"
	b, network := scheduleUITestBot(t, shifted)
	if _, err := b.schedules.SaveDraft(context.Background(), schedules.Draft{UserID: 1, ChatID: 1, Plan: plan, Stage: "review"}); err != nil {
		t.Fatal(err)
	}
	scheduleUIUpdate(t, b, telegram.Update{Message: scheduleUIMessage(10, "Сдвинь занятие на час позже")})
	draft, err := b.schedules.GetDraft(context.Background(), 1, 1)
	if err != nil || len(draft.Notes) != 0 || draft.Plan.Lessons[0].Start != "12:00" {
		t.Fatalf("first edit not consumed: %+v, %v", draft, err)
	}
	network.mu.Lock()
	network.plan.Lessons[0].Room = "425"
	network.mu.Unlock()
	second := "Кабинет поменяй на 425"
	scheduleUIUpdate(t, b, telegram.Update{Message: scheduleUIMessage(11, second)})
	if len(network.requests) != 2 {
		t.Fatalf("expected two independent edits, got %d", len(network.requests))
	}
	var content string
	if err := json.Unmarshal(network.requests[1].Messages[1].Content, &content); err != nil {
		t.Fatal(err)
	}
	var source struct {
		CurrentPlan schedules.Plan `json:"current_plan"`
		Notes       []string       `json:"notes"`
	}
	if err := json.Unmarshal([]byte(content), &source); err != nil {
		t.Fatal(err)
	}
	if len(source.Notes) != 1 || source.Notes[0] != second || source.CurrentPlan.Lessons[0].Start != "12:00" {
		t.Fatalf("relative edit replayed or updated plan lost: %+v", source)
	}
}

func TestScheduleFailedCorrectionCannotActivatePreviousPlan(t *testing.T) {
	plan := scheduleUITestPlan(t)
	invalid := scheduleUITestPlan(t)
	invalid.Lessons[0].Start = "unreadable"
	b, network := scheduleUITestBot(t, invalid)
	if _, err := b.schedules.SaveDraft(context.Background(), schedules.Draft{UserID: 1, ChatID: 1, Plan: plan, Stage: "review"}); err != nil {
		t.Fatal(err)
	}
	scheduleUIUpdate(t, b, telegram.Update{Message: scheduleUIMessage(10, "Начало пары изменилось, исправь")})
	draft, err := b.schedules.GetDraft(context.Background(), 1, 1)
	if err != nil || draft.Stage != "collect" || len(draft.Notes) != 1 {
		t.Fatalf("failed correction left stale confirmation available: %+v, %v", draft, err)
	}
	if !strings.Contains(network.messages[len(network.messages)-1].Text, resources.Get("schedule.recognition_failed")) {
		t.Fatal("recognition error not explained")
	}
	scheduleUIUpdate(t, b, telegram.Update{CallbackQuery: scheduleUICallback(fmt.Sprintf("sch:activate:%d:30", draft.Revision))})
	if _, err := b.schedules.GetActive(context.Background(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old plan activated despite pending failed correction: %v", err)
	}
	if network.messages[len(network.messages)-1].Text != resources.Get("schedule.stale") {
		t.Fatal("pending-correction activation did not return stale response")
	}
}

func TestScheduleDeliveryRechecksClaimAfterSkipOrOff(t *testing.T) {
	for _, action := range []string{"send", "skip", "off"} {
		t.Run(action, func(t *testing.T) {
			plan := scheduleUITestPlan(t)
			b, network := scheduleUITestBot(t, plan)
			ctx := context.Background()
			draft, err := b.schedules.SaveDraft(ctx, schedules.Draft{UserID: 1, ChatID: 1, Plan: plan, Stage: "review"})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.schedules.Activate(ctx, 1, 1, draft.Revision, 30, time.Now()); err != nil {
				t.Fatal(err)
			}
			occurrences, err := schedules.Generate(plan, time.Now())
			if err != nil || len(occurrences) != 1 {
				t.Fatalf("fixture occurrence: %+v, %v", occurrences, err)
			}
			trigger := occurrences[0].StartAt.Add(-30 * time.Minute)
			due, err := b.schedules.TakeDue(ctx, trigger, 50)
			if err != nil || len(due) != 1 {
				t.Fatalf("claim fixture: %+v, %v", due, err)
			}
			switch action {
			case "skip":
				if n, err := b.schedules.Skip(ctx, 1, 1, plan.Lessons[0].Date, "11:00"); err != nil || n != 1 {
					t.Fatalf("skip claimed pair: %d, %v", n, err)
				}
			case "off":
				if err := b.schedules.Disable(ctx, 1, 1); err != nil {
					t.Fatal(err)
				}
			}
			b.deliverSchedule(ctx, due[0])
			want := 0
			if action == "send" {
				want = 1
			}
			if len(network.messages) != want {
				t.Fatalf("%s: sent %d notifications, want %d", action, len(network.messages), want)
			}
			if repeat, err := b.schedules.TakeDue(ctx, trigger, 50); err != nil || len(repeat) != 0 {
				t.Fatalf("delivered/canceled notification claimed again: %+v, %v", repeat, err)
			}
		})
	}
}
