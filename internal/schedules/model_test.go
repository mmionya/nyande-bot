package schedules

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func calendarNow() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func datedPlan() Plan {
	return Plan{Timezone: "Asia/Almaty", LeadMinutes: 30, Lessons: []Lesson{
		{Date: "2026-10-15", Start: "11:00", End: "12:15", Subject: "Основы менеджмента", Room: "304", Building: "Главный учебный корпус"},
		{Date: "2026-10-15", Start: "12:30", End: "13:45", Subject: "Основы менеджмента", Room: "425", Building: "Главный учебный корпус"},
	}}
}

func weeklyPlan() Plan {
	return Plan{Timezone: "Asia/Almaty", CycleWeeks: 2, AnchorDate: "2026-10-12", StartDate: "2026-10-12", EndDate: "2026-11-01", LeadMinutes: 10, Lessons: []Lesson{
		{Week: 1, Weekday: 1, Start: "16:00", End: "16:50", Subject: "Дискретная математика", Room: "C1.1.328L"},
		{Week: 1, Weekday: 1, Start: "17:00", End: "17:50", Subject: "Дискретная математика", Room: "C1.1.328L"},
	}}
}

func TestNormalizeScreenshotOverlap(t *testing.T) {
	p := weeklyPlan()
	l := Lesson{Week: 1, Weekday: 2, Start: "15:00", End: "15:50", Subject: "Введение в функциональное программирование", Room: "C1.1.352K", Kind: "Практические занятия"}
	p.Lessons = append(p.Lessons, l)
	dup := l
	dup.Subject = "  ВВЕДЕНИЕ  в функциональное\n программирование "
	dup.Room = " c1.1.352k "
	dup.Kind = " практические  занятия "
	p.Lessons = append(p.Lessons, dup)
	l.Start, l.End = "16:00", "16:50"
	p.Lessons = append(p.Lessons, l)
	n, err := Normalize(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Lessons) != 4 {
		t.Fatalf("got %d rows, want 4: %+v", len(n.Lessons), n.Lessons)
	}
	if len(p.Lessons) != 5 || p.Lessons[3].Room != " c1.1.352k " {
		t.Fatal("Normalize mutated caller's draft")
	}
	occ, err := Generate(n, calendarNow())
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 8 {
		t.Fatalf("got %d occurrences, want two weeks of four", len(occ))
	}
	if occ[0].StartAt.Format("2006-01-02 15:04") != "2026-10-12 16:00" || occ[1].StartAt.Format("15:04") != "17:00" {
		t.Fatalf("adjacent classes merged: %+v", occ[:2])
	}
}

func TestDatedScreenshotIsOneDayAndChangesRooms(t *testing.T) {
	occ, err := Generate(datedPlan(), calendarNow())
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 2 {
		t.Fatalf("got %d occurrences, want 2", len(occ))
	}
	for i, want := range []string{"2026-10-15 10:30", "2026-10-15 12:00"} {
		if got := occ[i].TriggerAt.Format("2006-01-02 15:04"); got != want {
			t.Errorf("trigger %d=%s want %s", i, got, want)
		}
	}
	if occ[0].Lesson.Room != "304" || occ[1].Lesson.Room != "425" {
		t.Fatalf("rooms changed: %+v", occ)
	}
	if !occ[0].StartAt.UTC().Equal(time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("incorrect timezone: %s", occ[0].StartAt)
	}
	if len(Issues(datedPlan())) != 0 {
		t.Fatalf("dated plan requested recurring metadata: %v", Issues(datedPlan()))
	}
}

func TestAlternatingWeeksAcrossNewYearAndBeforeAnchor(t *testing.T) {
	p := weeklyPlan()
	p.AnchorDate = "2027-01-04"
	p.StartDate, p.EndDate = "2026-12-21", "2027-01-18"
	p.Lessons = p.Lessons[:1]
	occ, err := Generate(p, calendarNow())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range occ {
		got = append(got, o.StartAt.Format(dateLayout))
	}
	want := []string{"2026-12-21", "2027-01-04", "2027-01-18"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// January 4 is week 1 by explicit anchor even though ISO week changes at
	// the year boundary; no ISO odd/even rule may replace this anchor.
}

func TestEveryWeekAndLongerCycle(t *testing.T) {
	p := weeklyPlan()
	p.CycleWeeks = 3
	p.StartDate, p.EndDate = "2026-10-12", "2026-11-02"
	p.Lessons = p.Lessons[:1]
	p.Lessons[0].Week = 0
	occ, err := Generate(p, calendarNow())
	if err != nil || len(occ) != 4 {
		t.Fatalf("all weeks: occurrences=%d error=%v", len(occ), err)
	}
	p.Lessons[0].Week = 3
	occ, err = Generate(p, calendarNow())
	if err != nil || len(occ) != 1 || occ[0].StartAt.Format(dateLayout) != "2026-10-26" {
		t.Fatalf("third week: %+v %v", occ, err)
	}
	p.CycleWeeks = 1
	p.Lessons[0].Week = 1
	p.AnchorDate = ""
	occ, err = Generate(p, calendarNow())
	if err != nil || len(occ) != 4 {
		t.Fatalf("one-week no-anchor cycle: occurrences=%d error=%v", len(occ), err)
	}
}

func TestLeadMinutesAndStrictFutureTriggers(t *testing.T) {
	for _, lead := range []int{10, 30, 60} {
		t.Run(strconv.Itoa(lead), func(t *testing.T) {
			p := datedPlan()
			p.LeadMinutes = lead
			p.Lessons = p.Lessons[:1]
			occ, err := Generate(p, calendarNow())
			if err != nil || len(occ) != 1 {
				t.Fatalf("occ=%+v err=%v", occ, err)
			}
			if got := occ[0].StartAt.Sub(occ[0].TriggerAt); got != time.Duration(lead)*time.Minute {
				t.Fatalf("lead=%s", got)
			}
			if due, err := Generate(p, occ[0].TriggerAt); err != nil || len(due) != 0 {
				t.Fatalf("already due reminder was generated: %+v %v", due, err)
			}
			if due, err := Generate(p, occ[0].TriggerAt.Add(-time.Nanosecond)); err != nil || len(due) != 1 {
				t.Fatalf("future reminder omitted: %+v %v", due, err)
			}
		})
	}
}

func TestUTCAndNotificationOnPreviousDay(t *testing.T) {
	p := Plan{Timezone: "UTC", LeadMinutes: 60, Lessons: []Lesson{{Date: "2026-10-15", Start: "00:30", Subject: "Ночное занятие"}}}
	occ, err := Generate(p, calendarNow())
	if err != nil || len(occ) != 1 {
		t.Fatalf("occurrences=%+v error=%v", occ, err)
	}
	if got := occ[0].TriggerAt.Format(time.RFC3339); got != "2026-10-14T23:30:00Z" {
		t.Fatalf("got %s, want previous-day notification", got)
	}
}

func TestDSTMaintainsLocalClockAndWeekCycle(t *testing.T) {
	p := weeklyPlan()
	p.Timezone = "Europe/Berlin"
	p.AnchorDate, p.StartDate, p.EndDate = "2026-03-16", "2026-03-16", "2026-04-13"
	p.Lessons = p.Lessons[:1]
	p.Lessons[0].Start, p.Lessons[0].End = "10:00", "10:50"
	occ, err := Generate(p, calendarNow())
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 3 {
		t.Fatalf("got %d classes, want 3", len(occ))
	}
	for i, want := range []string{"2026-03-16T09:00:00Z", "2026-03-30T08:00:00Z", "2026-04-13T08:00:00Z"} {
		if got := occ[i].StartAt.UTC().Format(time.RFC3339); got != want {
			t.Errorf("got %s want %s", got, want)
		}
		if occ[i].StartAt.Hour() != 10 {
			t.Errorf("wall clock changed: %s", occ[i].StartAt)
		}
	}
}

func TestDSTMissingAndAmbiguousTimesRejected(t *testing.T) {
	for _, date := range []string{"2026-03-29", "2026-10-25"} {
		p := Plan{Timezone: "Europe/Berlin", LeadMinutes: 10, Lessons: []Lesson{{Date: date, Start: "02:30", Subject: "Математика"}}}
		if _, err := Generate(p, calendarNow()); err == nil {
			t.Fatalf("accepted nonexistent/ambiguous %s 02:30", date)
		}
		if len(Issues(p)) == 0 {
			t.Fatalf("no issue for %s", date)
		}
	}
	// Lord Howe repeats only 30 minutes, so checking +/- one hour is insufficient.
	p := Plan{Timezone: "Australia/Lord_Howe", LeadMinutes: 10, Lessons: []Lesson{{Date: "2026-04-05", Start: "01:45", Subject: "Математика"}}}
	if _, err := Generate(p, calendarNow()); err == nil {
		t.Fatal("accepted ambiguous half-hour DST transition")
	}
}

func TestNormalizeAllowsDraftButGenerateRejectsMissingMetadata(t *testing.T) {
	p := Plan{Lessons: []Lesson{{Week: 1, Weekday: 1, Start: "16:00", Subject: "Математика"}}}
	n, err := Normalize(p)
	if err != nil {
		t.Fatal(err)
	}
	if n.Timezone != "" || n.StartDate != "" || n.AnchorDate != "" || n.LeadMinutes != 0 || n.CycleWeeks != 0 {
		t.Fatalf("inferred metadata: %+v", n)
	}
	if len(Issues(n)) < 5 {
		t.Fatalf("missing metadata not identified: %v", Issues(n))
	}
	if _, err := Generate(n, calendarNow()); err == nil {
		t.Fatal("accepted incomplete draft")
	}
	p = weeklyPlan()
	p.AnchorDate = ""
	if _, err := Generate(p, calendarNow()); err == nil {
		t.Fatal("accepted alternating cycle with no anchor")
	}
	p = datedPlan()
	p.Questions = []string{"  Какой  корпус? ", "Какой корпус?"}
	n, err = Normalize(p)
	if err != nil || len(n.Questions) != 1 {
		t.Fatalf("questions=%v error=%v", n.Questions, err)
	}
	if _, err := Generate(n, calendarNow()); err == nil || !strings.Contains(err.Error(), "Какой корпус?") {
		t.Fatalf("unresolved question was not blocking: %v", err)
	}
}

func TestMalformedValuesRejected(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Plan)
	}{
		{"timezone", func(p *Plan) { p.Timezone = "Mars/Nowhere" }},
		{"local timezone", func(p *Plan) { p.Timezone = "Local" }},
		{"cycle", func(p *Plan) { p.CycleWeeks = 5 }},
		{"negative cycle", func(p *Plan) { p.CycleWeeks = -1 }},
		{"lead", func(p *Plan) { p.LeadMinutes = 15 }},
		{"anchor weekday", func(p *Plan) { p.AnchorDate = "2026-10-13" }},
		{"invalid date", func(p *Plan) { p.StartDate = "2026-02-30" }},
		{"no year", func(p *Plan) { p.Lessons[0].Date = "15 октября" }},
		{"invalid year", func(p *Plan) { p.Lessons[0].Date = "0000-10-15" }},
		{"reversed range", func(p *Plan) { p.StartDate = "2026-11-01"; p.EndDate = "2026-10-01" }},
		{"too long range", func(p *Plan) { p.StartDate = "2026-01-01"; p.EndDate = "2027-01-02" }},
		{"clock", func(p *Plan) { p.Lessons[0].Start = "25:00" }},
		{"clock ambiguous format", func(p *Plan) { p.Lessons[0].Start = "9:00" }},
		{"overnight", func(p *Plan) { p.Lessons[0].End = "10:59" }},
		{"weekday", func(p *Plan) { p.Lessons[0].Weekday = 8 }},
		{"week", func(p *Plan) { p.Lessons[0].Week = 5 }},
		{"dated repeat", func(p *Plan) { p.Lessons[0].Weekday = 4 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := datedPlan()
			tc.change(&p)
			if _, err := Normalize(p); err == nil {
				t.Fatal("malformed draft accepted")
			}
			if len(Issues(p)) == 0 {
				t.Fatal("malformed draft has no issue")
			}
		})
	}
	p := weeklyPlan()
	p.CycleWeeks = 1
	p.Lessons[0].Week = 2
	if _, err := Normalize(p); err == nil {
		t.Fatal("accepted week outside cycle")
	}
}

func TestSameStartConflictsAndEquivalentSources(t *testing.T) {
	p := datedPlan()
	other := p.Lessons[0]
	other.Room = "425"
	p.Lessons = append(p.Lessons, other)
	n, err := Normalize(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Lessons) != 3 {
		t.Fatal("different rooms were silently deduplicated")
	}
	if _, err := Generate(n, calendarNow()); err == nil {
		t.Fatal("conflicting rooms accepted")
	}
	if len(Issues(n)) == 0 {
		t.Fatal("conflicting rooms have no issue")
	}
	p = weeklyPlan()
	p.StartDate, p.EndDate = "2026-10-12", "2026-10-12"
	copy := p.Lessons[0]
	copy.Date = "2026-10-12"
	copy.Week, copy.Weekday = 0, 0
	p.Lessons = append(p.Lessons, copy)
	occ, err := Generate(p, calendarNow())
	if err != nil || len(occ) != 2 {
		t.Fatalf("exact dated and weekly copies are one class: %+v %v", occ, err)
	}
	p.Lessons[len(p.Lessons)-1].Subject = "Физика"
	if _, err := Generate(p, calendarNow()); err == nil {
		t.Fatal("conflicting weekly/dated classes accepted")
	}
}

func TestKeysStableWhenReminderAndRoomAreCorrected(t *testing.T) {
	p := datedPlan()
	a, err := Generate(p, calendarNow())
	if err != nil {
		t.Fatal(err)
	}
	p.LeadMinutes = 60
	p.Lessons[0].Room = "301"
	p.Lessons[0].End = "12:00"
	p.Lessons[0].Kind = "Лекция"
	b, err := Generate(p, calendarNow())
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Key != b[0].Key {
		t.Fatal("correction changed persistent occurrence identity")
	}
	if a[0].Key == a[1].Key {
		t.Fatal("separate start times have same identity")
	}
}

func TestInclusiveRangeExpiredAndOptionalEnd(t *testing.T) {
	p := weeklyPlan()
	p.StartDate, p.EndDate = "2026-10-12", "2026-10-12"
	p.Lessons[0].End = ""
	occ, err := Generate(p, calendarNow())
	if err != nil || len(occ) != 2 {
		t.Fatalf("inclusive single date: %+v %v", occ, err)
	}
	if !occ[0].EndAt.IsZero() {
		t.Fatal("inferred missing end time")
	}
	occ, err = Generate(p, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC))
	if err != nil || len(occ) != 0 {
		t.Fatalf("expired schedule: %+v %v", occ, err)
	}
	p = datedPlan()
	p.StartDate = "2026-10-16"
	occ, err = Generate(p, calendarNow())
	if err != nil || len(occ) != 0 {
		t.Fatalf("dated class outside specified range: %+v %v", occ, err)
	}
	p = weeklyPlan()
	p.StartDate, p.EndDate = "2026-01-01", "2027-01-01"
	if _, err = Generate(p, calendarNow()); err != nil {
		t.Fatalf("366-day inclusive range rejected: %v", err)
	}
}

func TestLessonCountBound(t *testing.T) {
	p := datedPlan()
	p.Lessons = make([]Lesson, 201)
	for i := range p.Lessons {
		p.Lessons[i] = Lesson{Date: "2026-10-15", Start: "11:00", Subject: "Математика"}
	}
	if _, err := Normalize(p); err == nil {
		t.Fatal("unbounded raw lesson list accepted")
	}
}
