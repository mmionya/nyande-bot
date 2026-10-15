// Package schedules validates recognized class schedules and resolves their
// local calendar dates into persistent notification times.
package schedules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	maxLessons     = 200
	maxOccurrences = 20000
	dateLayout     = "2006-01-02"
)

// Lesson is either a dated class or a weekly class. Week 0 means every week;
// weekday uses Monday=1 through Sunday=7. No dates or repeat rules are inferred.
type Lesson struct {
	Week     int    `json:"week"`
	Weekday  int    `json:"weekday"`
	Date     string `json:"date"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Subject  string `json:"subject"`
	Room     string `json:"room"`
	Building string `json:"building"`
	Kind     string `json:"kind"`
}

type Plan struct {
	Timezone    string   `json:"timezone"`
	CycleWeeks  int      `json:"cycle_weeks"`
	AnchorDate  string   `json:"anchor_date"`
	StartDate   string   `json:"start_date"`
	EndDate     string   `json:"end_date"`
	LeadMinutes int      `json:"lead_minutes"`
	Lessons     []Lesson `json:"lessons"`
	Questions   []string `json:"questions"`
}

type Occurrence struct {
	Key       string    `json:"key"`
	StartAt   time.Time `json:"start_at"`
	EndAt     time.Time `json:"end_at"`
	TriggerAt time.Time `json:"trigger_at"`
	Lesson    Lesson    `json:"lesson"`
}

// Normalize allows incomplete drafts, but rejects malformed values. It does
// not fill missing dates, cycle metadata, timezone or reminder preferences.
func Normalize(p Plan) (Plan, error) {
	p.Timezone = strings.TrimSpace(p.Timezone)
	p.AnchorDate = strings.TrimSpace(p.AnchorDate)
	p.StartDate = strings.TrimSpace(p.StartDate)
	p.EndDate = strings.TrimSpace(p.EndDate)
	if len(p.Lessons) > maxLessons {
		return Plan{}, fmt.Errorf("В расписании может быть не больше %d занятий.", maxLessons)
	}
	if p.Timezone != "" {
		if _, err := location(p.Timezone); err != nil {
			return Plan{}, err
		}
	}
	if p.CycleWeeks < 0 || p.CycleWeeks > 4 {
		return Plan{}, errors.New("Цикл расписания должен содержать от 1 до 4 недель.")
	}
	if p.LeadMinutes != 0 && p.LeadMinutes != 10 && p.LeadMinutes != 30 && p.LeadMinutes != 60 {
		return Plan{}, errors.New("Напоминание можно настроить за 10, 30 или 60 минут.")
	}
	for _, v := range []struct{ label, value string }{
		{"Дата начала цикла", p.AnchorDate}, {"Начало действия расписания", p.StartDate}, {"Конец действия расписания", p.EndDate},
	} {
		if v.value != "" {
			if _, err := parseDate(v.value); err != nil {
				return Plan{}, fmt.Errorf("%s: укажите полную дату ГГГГ-ММ-ДД.", v.label)
			}
		}
	}
	if p.AnchorDate != "" {
		a, _ := parseDate(p.AnchorDate)
		if a.Weekday() != time.Monday {
			return Plan{}, errors.New("Начало первой недели цикла должно приходиться на понедельник.")
		}
	}
	if p.StartDate != "" && p.EndDate != "" {
		start, _ := parseDate(p.StartDate)
		end, _ := parseDate(p.EndDate)
		if end.Before(start) {
			return Plan{}, errors.New("Конец действия расписания не может быть раньше начала.")
		}
		if end.Sub(start)/(24*time.Hour) >= 366 {
			return Plan{}, errors.New("Период действия расписания не должен превышать 366 дней.")
		}
	}
	lessons := make([]Lesson, 0, len(p.Lessons))
	seen := make(map[string]bool)
	for i, l := range p.Lessons {
		l.Date = strings.TrimSpace(l.Date)
		l.Start = strings.TrimSpace(l.Start)
		l.End = strings.TrimSpace(l.End)
		l.Subject = cleanText(l.Subject)
		l.Room = cleanText(l.Room)
		l.Building = cleanText(l.Building)
		l.Kind = cleanText(l.Kind)
		if l.Week < 0 || l.Week > 4 || (p.CycleWeeks > 0 && l.Week > p.CycleWeeks) {
			return Plan{}, fmt.Errorf("Занятие %d: номер недели не входит в цикл расписания.", i+1)
		}
		if l.Weekday < 0 || l.Weekday > 7 {
			return Plan{}, fmt.Errorf("Занятие %d: день недели должен быть от 1 (понедельник) до 7 (воскресенье).", i+1)
		}
		if l.Date != "" {
			if _, err := parseDate(l.Date); err != nil {
				return Plan{}, fmt.Errorf("Занятие %d: укажите полную дату ГГГГ-ММ-ДД, включая год.", i+1)
			}
			if l.Week != 0 || l.Weekday != 0 {
				return Plan{}, fmt.Errorf("Занятие %d: выберите конкретную дату или повторение по дням недели.", i+1)
			}
		}
		for _, value := range []string{l.Start, l.End} {
			if value != "" {
				if _, _, err := parseClock(value); err != nil {
					return Plan{}, fmt.Errorf("Занятие %d: время должно быть в формате ЧЧ:ММ (00:00–23:59).", i+1)
				}
			}
		}
		if l.Start != "" && l.End != "" && l.End <= l.Start {
			return Plan{}, fmt.Errorf("Занятие %d: конец должен быть позже начала в тот же день.", i+1)
		}
		key := lessonKey(l)
		if !seen[key] {
			seen[key] = true
			lessons = append(lessons, l)
		}
	}
	sort.SliceStable(lessons, func(i, j int) bool {
		a, b := lessons[i], lessons[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Week != b.Week {
			return a.Week < b.Week
		}
		if a.Weekday != b.Weekday {
			return a.Weekday < b.Weekday
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return lessonKey(a) < lessonKey(b)
	})
	p.Lessons = lessons
	p.Questions = uniqueText(p.Questions)
	return p, nil
}

// Issues returns the missing decisions and validation problems that must be
// resolved before a draft can be activated. It never assumes the current year.
func Issues(p Plan) []string {
	n, err := Normalize(p)
	if err != nil {
		return []string{err.Error()}
	}
	issues := metadataIssues(n)
	if len(issues) == 0 {
		if _, err := expand(n); err != nil {
			issues = append(issues, err.Error())
		}
	}
	return uniqueText(issues)
}

func metadataIssues(p Plan) []string {
	issues := append([]string(nil), p.Questions...)
	if len(p.Lessons) == 0 {
		issues = append(issues, "Добавьте хотя бы одно занятие.")
	}
	if p.Timezone == "" {
		issues = append(issues, "Укажите часовой пояс занятий, например Asia/Almaty.")
	}
	if p.LeadMinutes == 0 {
		issues = append(issues, "Выберите напоминания за 10, 30 или 60 минут.")
	}
	weekly := false
	for i, l := range p.Lessons {
		if l.Subject == "" {
			issues = append(issues, fmt.Sprintf("Занятие %d: укажите название предмета.", i+1))
		}
		if l.Start == "" {
			issues = append(issues, fmt.Sprintf("Занятие %d: укажите время начала.", i+1))
		}
		if l.Date == "" {
			weekly = true
			if l.Weekday == 0 {
				issues = append(issues, fmt.Sprintf("Занятие %d: укажите конкретную дату или день недели.", i+1))
			}
		}
	}
	if weekly {
		if p.CycleWeeks == 0 {
			issues = append(issues, "Укажите число недель в цикле: от 1 до 4.")
		}
		if p.CycleWeeks > 1 && p.AnchorDate == "" {
			issues = append(issues, "Укажите дату понедельника первой недели цикла.")
		}
		if p.StartDate == "" {
			issues = append(issues, "Укажите дату начала действия расписания.")
		}
		if p.EndDate == "" {
			issues = append(issues, "Укажите дату окончания действия расписания.")
		}
	}
	return issues
}

// Generate resolves and checks all configured classes, then returns only
// notifications strictly after now. Past or already-due reminders are skipped.
func Generate(p Plan, now time.Time) ([]Occurrence, error) {
	n, err := Normalize(p)
	if err != nil {
		return nil, err
	}
	if issues := metadataIssues(n); len(issues) > 0 {
		return nil, errors.New(strings.Join(issues, "\n"))
	}
	all, err := expand(n)
	if err != nil {
		return nil, err
	}
	future := make([]Occurrence, 0, len(all))
	for _, o := range all {
		if o.TriggerAt.After(now) {
			future = append(future, o)
		}
	}
	return future, nil
}

func expand(p Plan) ([]Occurrence, error) {
	loc, err := location(p.Timezone)
	if err != nil {
		return nil, err
	}
	result := make([]Occurrence, 0)
	seen := make(map[string]bool)
	starts := make(map[int64]string)
	add := func(day time.Time, l Lesson) error {
		date := day.Format(dateLayout)
		if (p.StartDate != "" && date < p.StartDate) || (p.EndDate != "" && date > p.EndDate) {
			return nil
		}
		start, err := localTime(day, l.Start, loc)
		if err != nil {
			return fmt.Errorf("%s, %s: %w", date, l.Subject, err)
		}
		var end time.Time
		if l.End != "" {
			end, err = localTime(day, l.End, loc)
			if err != nil {
				return fmt.Errorf("%s, %s: %w", date, l.Subject, err)
			}
			if !end.After(start) {
				return fmt.Errorf("%s, %s: конец занятия должен быть позже начала.", date, l.Subject)
			}
		}
		// Source row metadata is excluded: a weekly row and its exact dated
		// copy represent the same actual class. Lead time does not change identity.
		identity := l
		identity.Date, identity.Week, identity.Weekday = "", 0, 0
		fullIdentity := start.UTC().Format(time.RFC3339) + ":" + lessonKey(identity)
		if seen[fullIdentity] {
			return nil
		}
		if other, ok := starts[start.Unix()]; ok {
			return fmt.Errorf("%s в %s указаны разные занятия или кабинеты (%s; %s). Уточните расписание.", date, l.Start, other, classLabel(l))
		}
		seen[fullIdentity] = true
		starts[start.Unix()] = classLabel(l)
		if len(result) >= maxOccurrences {
			return fmt.Errorf("Расписание содержит больше %d событий; сократите период или число занятий.", maxOccurrences)
		}
		// Correcting a room, building, end time or advance notice must not
		// send another reminder for a class whose notification already ran.
		sum := sha256.Sum256([]byte(start.UTC().Format(time.RFC3339) + ":" + strings.ToLower(cleanText(l.Subject))))
		key := hex.EncodeToString(sum[:])
		result = append(result, Occurrence{Key: key, StartAt: start, EndAt: end, TriggerAt: start.Add(-time.Duration(p.LeadMinutes) * time.Minute), Lesson: l})
		return nil
	}
	for _, l := range p.Lessons {
		if l.Date != "" {
			day, _ := parseDate(l.Date)
			if err := add(day, l); err != nil {
				return nil, err
			}
			continue
		}
		start, _ := parseDate(p.StartDate)
		end, _ := parseDate(p.EndDate)
		anchor, _ := parseDate(p.AnchorDate)
		// Days are iterated in a UTC calendar, then resolved in the user's zone.
		// This avoids 23/25-hour DST days changing cycle-week parity.
		for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
			weekday := (int(day.Weekday())+6)%7 + 1
			if weekday != l.Weekday {
				continue
			}
			if l.Week > 0 && p.CycleWeeks > 1 {
				monday := day.AddDate(0, 0, -(weekday - 1))
				weeks := int((monday.Unix()-anchor.Unix())/86400) / 7
				cycleWeek := ((weeks%p.CycleWeeks)+p.CycleWeeks)%p.CycleWeeks + 1
				if cycleWeek != l.Week {
					continue
				}
			}
			if err := add(day, l); err != nil {
				return nil, err
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].TriggerAt.Equal(result[j].TriggerAt) {
			return result[i].Key < result[j].Key
		}
		return result[i].TriggerAt.Before(result[j].TriggerAt)
	})
	return result, nil
}

func location(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("Укажите часовой пояс IANA, например Asia/Almaty или Europe/Berlin.")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("Неизвестный часовой пояс %q; используйте название IANA, например Asia/Almaty.", name)
	}
	return loc, nil
}

func parseDate(value string) (time.Time, error) {
	date, err := time.Parse(dateLayout, value)
	if err != nil || date.Format(dateLayout) != value || date.Year() < 1 {
		return time.Time{}, errors.New("invalid calendar date")
	}
	return date, nil
}

func parseClock(value string) (int, int, error) {
	t, err := time.Parse("15:04", value)
	if err != nil || t.Format("15:04") != value {
		return 0, 0, errors.New("invalid clock time")
	}
	return t.Hour(), t.Minute(), nil
}

// localTime rejects both skipped and repeated wall-clock times. time.Date's
// choice around a DST transition is intentionally not used as an implicit rule.
func localTime(day time.Time, clock string, loc *time.Location) (time.Time, error) {
	hour, minute, err := parseClock(clock)
	if err != nil {
		return time.Time{}, err
	}
	year, month, date := day.Date()
	want := time.Date(year, month, date, hour, minute, 0, 0, time.UTC)
	guess := time.Date(year, month, date, hour, minute, 0, 0, loc)
	offsets := make(map[int]bool)
	for d := -48; d <= 48; d++ {
		_, offset := guess.Add(time.Duration(d) * time.Hour).Zone()
		offsets[offset] = true
	}
	var matches []time.Time
	for offset := range offsets {
		t := want.Add(-time.Duration(offset) * time.Second).In(loc)
		if t.Year() == year && t.Month() == month && t.Day() == date && t.Hour() == hour && t.Minute() == minute {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return time.Time{}, fmt.Errorf("время %s отсутствует в часовом поясе %s из-за перевода часов; уточните время занятия.", clock, loc)
	}
	if len(matches) > 1 {
		return time.Time{}, fmt.Errorf("время %s повторяется в часовом поясе %s при переводе часов; уточните время занятия.", clock, loc)
	}
	return matches[0], nil
}

func cleanText(value string) string { return strings.Join(strings.Fields(value), " ") }

func lessonKey(l Lesson) string {
	l.Subject = strings.ToLower(cleanText(l.Subject))
	l.Room = strings.ToLower(cleanText(l.Room))
	l.Building = strings.ToLower(cleanText(l.Building))
	l.Kind = strings.ToLower(cleanText(l.Kind))
	data, _ := json.Marshal(l)
	return string(data)
}

func uniqueText(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		value = cleanText(value)
		key := strings.ToLower(value)
		if value != "" && !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	return result
}

func classLabel(l Lesson) string {
	if l.Room != "" {
		return l.Subject + ", кабинет " + l.Room
	}
	return l.Subject
}
