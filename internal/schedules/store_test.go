package schedules

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func storeTestTime(t *testing.T, clock string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatal(err)
	}
	value, err := time.ParseInLocation("2006-01-02 15:04", "2026-10-15 "+clock, loc)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func storeTestPlan() Plan {
	return Plan{
		Timezone: "Asia/Almaty", LeadMinutes: 10,
		Lessons: []Lesson{
			{Date: "2026-10-15", Start: "11:00", End: "12:15", Subject: "Основы менеджмента", Room: "304"},
			{Date: "2026-10-15", Start: "12:30", End: "13:45", Subject: "Основы менеджмента", Room: "425"},
		},
	}
}

func openStoreForTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "schedules.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func saveStoreDraft(t *testing.T, s *Store, user, chat int64, plan Plan) Draft {
	t.Helper()
	d, err := s.SaveDraft(context.Background(), Draft{UserID: user, ChatID: chat, Plan: plan, Stage: "review"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func activateStoreDraft(t *testing.T, s *Store, d Draft, lead int, clock string) (Active, int) {
	t.Helper()
	a, n, err := s.Activate(context.Background(), d.UserID, d.ChatID, d.Revision, lead, storeTestTime(t, clock))
	if err != nil {
		t.Fatal(err)
	}
	return a, n
}

func TestStoreDraftRevisionsNeverReuseAndOwnerIsolation(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	d1 := saveStoreDraft(t, s, 1, 101, storeTestPlan())
	d2 := saveStoreDraft(t, s, 2, 202, storeTestPlan())
	if d2.Revision <= d1.Revision {
		t.Fatalf("revisions must increase globally: %d then %d", d1.Revision, d2.Revision)
	}
	if _, err := s.GetDraft(ctx, 1, 202); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another chat's draft exposed: %v", err)
	}
	if _, _, err := s.Activate(ctx, 2, 202, d1.Revision, 10, storeTestTime(t, "09:00")); !errors.Is(err, ErrDraftChanged) {
		t.Fatalf("foreign confirmation accepted: %v", err)
	}
	if err := s.DeleteDraft(ctx, 1, 101); err != nil {
		t.Fatal(err)
	}
	d3 := saveStoreDraft(t, s, 1, 101, storeTestPlan())
	if d3.Revision <= d2.Revision {
		t.Fatalf("revision reused after deletion: %+v", d3)
	}
	if _, _, err := s.Activate(ctx, 1, 101, d1.Revision, 10, storeTestTime(t, "09:00")); !errors.Is(err, ErrDraftChanged) {
		t.Fatalf("old confirmation accepted for new draft: %v", err)
	}
	a, n := activateStoreDraft(t, s, d3, 30, "09:00")
	if n != 2 || a.Plan.LeadMinutes != 30 {
		t.Fatalf("activation: %+v, count %d", a, n)
	}
	if _, err := s.GetDraft(ctx, 1, 101); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("activated draft remains: %v", err)
	}
	if _, err := s.GetActive(ctx, 2, 202); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("active schedule exposed to other owner: %v", err)
	}
	if _, err := s.GetDraft(ctx, 2, 202); err != nil {
		t.Fatalf("other user's draft was affected: %v", err)
	}
	if _, _, err := s.Activate(ctx, 1, 101, d3.Revision, 30, storeTestTime(t, "09:00")); !errors.Is(err, ErrDraftChanged) {
		t.Fatalf("replayed confirmation accepted: %v", err)
	}
}

func TestStoreActivationRollbackPreservesActiveDraftAndPending(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	oldDraft := saveStoreDraft(t, s, 1, 1, storeTestPlan())
	oldActive, _ := activateStoreDraft(t, s, oldDraft, 10, "09:00")
	newPlan := storeTestPlan()
	newPlan.Lessons[0].Room = "999"
	newDraft := saveStoreDraft(t, s, 1, 1, newPlan)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_schedule_insert BEFORE INSERT ON schedule_notifications BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Activate(ctx, 1, 1, newDraft.Revision, 30, storeTestTime(t, "09:00")); err == nil {
		t.Fatal("expected notification insert failure")
	}
	active, err := s.GetActive(ctx, 1, 1)
	if err != nil || active.Revision != oldActive.Revision {
		t.Fatalf("old active schedule was not preserved: %+v, %v", active, err)
	}
	draft, err := s.GetDraft(ctx, 1, 1)
	if err != nil || draft.Revision != newDraft.Revision {
		t.Fatalf("draft lost on failed activation: %+v, %v", draft, err)
	}
	due, err := s.TakeDue(ctx, storeTestTime(t, "10:50"), 50)
	if err != nil || len(due) != 1 || due[0].Lesson.Room != "304" || due[0].LeadMinutes != 10 {
		t.Fatalf("original pending notification lost on rollback: %+v, %v", due, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_schedule_insert`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Activate(ctx, 1, 1, newDraft.Revision, 30, storeTestTime(t, "10:51")); err != nil {
		t.Fatalf("draft cannot be activated after failure: %v", err)
	}
}

func TestStoreRejectsIncompleteActivationWithoutReplacingSchedule(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	old, _ := activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 10, "09:00")
	invalid := storeTestPlan()
	invalid.Timezone = "Invalid/Timezone"
	d := saveStoreDraft(t, s, 1, 1, invalid)
	if _, _, err := s.Activate(ctx, 1, 1, d.Revision, 10, storeTestTime(t, "09:00")); err == nil {
		t.Fatal("invalid timezone activated")
	}
	if _, _, err := s.Activate(ctx, 1, 1, d.Revision, 11, storeTestTime(t, "09:00")); err == nil {
		t.Fatal("unsupported lead activated")
	}
	a, err := s.GetActive(ctx, 1, 1)
	if err != nil || a.Revision != old.Revision {
		t.Fatalf("invalid draft replaced active schedule: %+v, %v", a, err)
	}
}

func TestStoreConsumedSurvivesRestartReimportAndLeadRoomChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedules.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := saveStoreDraft(t, s, 1, 1, storeTestPlan())
	activateStoreDraft(t, s, first, 60, "09:00")
	due, err := s.TakeDue(ctx, storeTestTime(t, "10:00"), 50)
	if err != nil || len(due) != 1 || due[0].Lesson.Room != "304" {
		t.Fatalf("first send: %+v, %v", due, err)
	}
	// No delivery acknowledgement is necessary; restarting immediately after a
	// possibly successful network send must not make the same class due again.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	due, err = s.TakeDue(ctx, storeTestTime(t, "10:01"), 50)
	if err != nil || len(due) != 0 {
		t.Fatalf("notification replayed after restart: %+v, %v", due, err)
	}
	if err := s.Disable(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	changed := storeTestPlan()
	changed.Lessons[0].Room = "999"
	reimport := saveStoreDraft(t, s, 1, 1, changed)
	if reimport.Revision <= first.Revision {
		t.Fatal("revision counter did not survive restart")
	}
	_, count := activateStoreDraft(t, s, reimport, 10, "10:01")
	if count != 1 {
		t.Fatalf("expected only the unsent second class, got %d", count)
	}
	due, err = s.TakeDue(ctx, storeTestTime(t, "10:50"), 50)
	if err != nil || len(due) != 0 {
		t.Fatalf("reimported class resent after lead/room changes: %+v, %v", due, err)
	}
	due, err = s.TakeDue(ctx, storeTestTime(t, "12:20"), 50)
	if err != nil || len(due) != 1 || due[0].Lesson.Room != "425" {
		t.Fatalf("remaining unsent class lost: %+v, %v", due, err)
	}
}

func TestStoreDoesNotDeliverBacklogAndAllowsFiveMinuteGrace(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 10, "09:00")
	due, err := s.TakeDue(ctx, storeTestTime(t, "10:56"), 50)
	if err != nil || len(due) != 0 {
		t.Fatalf("six-minute-old notification delivered: %+v, %v", due, err)
	}
	due, err = s.TakeDue(ctx, storeTestTime(t, "12:25"), 50)
	if err != nil || len(due) != 1 || due[0].Lesson.Start != "12:30" {
		t.Fatalf("notification within five minute grace lost: %+v, %v", due, err)
	}
	due, err = s.TakeDue(ctx, storeTestTime(t, "12:25"), 50)
	if err != nil || len(due) != 0 {
		t.Fatalf("repeated poll resent notification: %+v, %v", due, err)
	}
}

func TestStoreSkipAndDisableAreOwnerScoped(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 101, storeTestPlan()), 10, "09:00")
	activateStoreDraft(t, s, saveStoreDraft(t, s, 2, 202, storeTestPlan()), 10, "09:00")
	if n, err := s.Skip(ctx, 1, 202, "2026-10-15", "11:00"); err != nil || n != 0 {
		t.Fatalf("cross-owner skip: count %d, %v", n, err)
	}
	if n, err := s.Skip(ctx, 1, 101, "2026-10-15", "11:00"); err != nil || n != 1 {
		t.Fatalf("skip: count %d, %v", n, err)
	}
	if n, err := s.Skip(ctx, 1, 101, "2026-10-15", "11:00"); err != nil || n != 0 {
		t.Fatalf("repeated skip: count %d, %v", n, err)
	}
	due, err := s.TakeDue(ctx, storeTestTime(t, "10:50"), 50)
	if err != nil || len(due) != 1 || due[0].UserID != 2 {
		t.Fatalf("skip affected wrong owner: %+v, %v", due, err)
	}
	if err := s.Disable(ctx, 2, 202); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetActive(ctx, 2, 202); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled plan remains active: %v", err)
	}
	due, err = s.TakeDue(ctx, storeTestTime(t, "12:20"), 50)
	if err != nil || len(due) != 1 || due[0].UserID != 1 {
		t.Fatalf("disable affected wrong owner: %+v, %v", due, err)
	}
}

func TestStoreConcurrentPollsConsumeOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 10, "09:00")
	now := storeTestTime(t, "10:50")
	results := make(chan []Notification, 8)
	errorsCh := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			due, err := s.TakeDue(ctx, now, 50)
			results <- due
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	count := 0
	for result := range results {
		count += len(result)
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("concurrent workers claimed %d copies", count)
	}
}

func TestStoreDraftBounds(t *testing.T) {
	s := openStoreForTest(t)
	for _, tc := range []struct {
		name  string
		draft Draft
	}{
		{"too many inputs", Draft{UserID: 1, ChatID: 1, Inputs: make([]Input, 13)}},
		{"notes too large", Draft{UserID: 1, ChatID: 1, Notes: []string{strings.Repeat("a", 16*1024)}}},
		{"draft too large", Draft{UserID: 1, ChatID: 1, Inputs: []Input{{Text: strings.Repeat("a", 1024*1024)}}}},
		{"invalid user", Draft{UserID: 0, ChatID: 1}},
		{"non-private chat", Draft{UserID: 1, ChatID: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SaveDraft(context.Background(), tc.draft); err == nil {
				t.Fatal("invalid draft accepted")
			}
		})
	}
	if _, err := s.GetDraft(context.Background(), 1, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed draft save wrote a row: %v", err)
	}
}

func TestStoreDraftSaveRejectsStaleUpdatesAndDeletedDrafts(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	original := saveStoreDraft(t, s, 1, 1, storeTestPlan())
	updated := original
	updated.Notes = []string{"latest clarification"}
	updated, err := s.SaveDraft(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	original.Notes = []string{"stale recognition result"}
	if _, err := s.SaveDraft(ctx, original); !errors.Is(err, ErrDraftChanged) {
		t.Fatalf("stale save overwrote latest draft: %v", err)
	}
	actual, err := s.GetDraft(ctx, 1, 1)
	if err != nil || actual.Revision != updated.Revision || len(actual.Notes) != 1 || actual.Notes[0] != "latest clarification" {
		t.Fatalf("draft changed after rejected save: %+v, %v", actual, err)
	}
	if err := s.DeleteDraft(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDraft(ctx, updated); !errors.Is(err, ErrDraftChanged) {
		t.Fatalf("in-flight recognition resurrected deleted draft: %v", err)
	}
	replacement := saveStoreDraft(t, s, 1, 1, storeTestPlan())
	if _, err := s.SaveDraft(ctx, updated); !errors.Is(err, ErrDraftChanged) {
		t.Fatalf("old draft overwrote intentionally replaced draft: %v", err)
	}
	actual, err = s.GetDraft(ctx, 1, 1)
	if err != nil || actual.Revision != replacement.Revision {
		t.Fatalf("replacement draft lost: %+v, %v", actual, err)
	}
}

func TestStoreSkippedClassStaysCanceledAfterReimport(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 10, "09:00")
	if n, err := s.Skip(ctx, 1, 1, "2026-10-15", "11:00"); err != nil || n != 1 {
		t.Fatalf("skip: %d, %v", n, err)
	}
	changed := storeTestPlan()
	changed.Lessons[0].Room = "999"
	_, count := activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, changed), 30, "09:00")
	if count != 1 {
		t.Fatalf("reimport restored skipped class: %d pending notifications", count)
	}
	due, err := s.TakeDue(ctx, storeTestTime(t, "10:30"), 50)
	if err != nil || len(due) != 0 {
		t.Fatalf("skipped class delivered after reimport: %+v, %v", due, err)
	}
	due, err = s.TakeDue(ctx, storeTestTime(t, "12:00"), 50)
	if err != nil || len(due) != 1 || due[0].Lesson.Room != "425" {
		t.Fatalf("unskipped class affected: %+v, %v", due, err)
	}
}

func TestStoreCanDeliverRejectsCanceledOrReplacedClaims(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"skip", "off", "replace"} {
		t.Run(action, func(t *testing.T) {
			s := openStoreForTest(t)
			active, _ := activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 10, "09:00")
			due, err := s.TakeDue(ctx, storeTestTime(t, "10:50"), 50)
			if err != nil || len(due) != 1 || due[0].Revision != active.Revision {
				t.Fatalf("claim lacks active revision: %+v, %v", due, err)
			}
			claim := due[0]
			if ok, err := s.CanDeliver(ctx, claim); err != nil || !ok {
				t.Fatalf("valid claimed notification rejected: %t, %v", ok, err)
			}
			foreign := claim
			foreign.UserID = 2
			if ok, err := s.CanDeliver(ctx, foreign); err != nil || ok {
				t.Fatalf("foreign notification permitted: %t, %v", ok, err)
			}
			switch action {
			case "skip":
				if n, err := s.Skip(ctx, 1, 1, "2026-10-15", "11:00"); err != nil || n != 1 {
					t.Fatalf("cannot skip already claimed notification: %d, %v", n, err)
				}
			case "off":
				if err := s.Disable(ctx, 1, 1); err != nil {
					t.Fatal(err)
				}
			case "replace":
				activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 30, "10:51")
			}
			if ok, err := s.CanDeliver(ctx, claim); err != nil || ok {
				t.Fatalf("queued canceled/replaced notification permitted: %t, %v", ok, err)
			}
		})
	}
}

func TestStorePastOnlyDraftCannotReplaceActiveSchedule(t *testing.T) {
	ctx := context.Background()
	s := openStoreForTest(t)
	active, _ := activateStoreDraft(t, s, saveStoreDraft(t, s, 1, 1, storeTestPlan()), 10, "09:00")
	past := storeTestPlan()
	for i := range past.Lessons {
		past.Lessons[i].Date = "2026-10-14"
	}
	draft := saveStoreDraft(t, s, 1, 1, past)
	if _, _, err := s.Activate(ctx, 1, 1, draft.Revision, 30, storeTestTime(t, "09:00")); err == nil {
		t.Fatal("past-only draft activated")
	}
	actual, err := s.GetActive(ctx, 1, 1)
	if err != nil || actual.Revision != active.Revision {
		t.Fatalf("past-only draft removed active schedule: %+v, %v", actual, err)
	}
	if retained, err := s.GetDraft(ctx, 1, 1); err != nil || retained.Revision != draft.Revision {
		t.Fatalf("rejected draft not retained: %+v, %v", retained, err)
	}
	due, err := s.TakeDue(ctx, storeTestTime(t, "10:50"), 50)
	if err != nil || len(due) != 1 {
		t.Fatalf("old notification lost: %+v, %v", due, err)
	}
}
