package schedules

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrDraftChanged also covers a confirmation button whose draft was removed.
var ErrDraftChanged = errors.New("schedule draft has changed or no longer exists")

type Input struct {
	FileID    string `json:"file_id,omitempty"`
	MIME      string `json:"mime,omitempty"`
	Text      string `json:"text,omitempty"`
	MessageID int    `json:"message_id,omitempty"`
	GroupID   string `json:"group_id,omitempty"`
}

type Draft struct {
	UserID              int64    `json:"user_id"`
	ChatID              int64    `json:"chat_id"`
	Revision            int64    `json:"revision"`
	Plan                Plan     `json:"plan"`
	Inputs              []Input  `json:"inputs,omitempty"`
	Notes               []string `json:"notes,omitempty"`
	Stage               string   `json:"stage"`
	CollectionMessageID int      `json:"collection_message_id,omitempty"`
}

type Active struct {
	UserID   int64 `json:"user_id"`
	ChatID   int64 `json:"chat_id"`
	Revision int64 `json:"revision"`
	Plan     Plan  `json:"plan"`
}

type Notification struct {
	ID          int64
	UserID      int64
	ChatID      int64
	Revision    int64
	StartAt     time.Time
	TriggerAt   time.Time
	Lesson      Lesson
	Timezone    string
	LeadMinutes int
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("schedule database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create schedule database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS schedule_metadata (name TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO schedule_metadata(name, value) VALUES ('revision', 0)`,
		`CREATE TABLE IF NOT EXISTS schedule_drafts (
			user_id INTEGER NOT NULL, chat_id INTEGER NOT NULL,
			revision INTEGER NOT NULL, data TEXT NOT NULL,
			PRIMARY KEY(user_id, chat_id)
		)`,
		`CREATE TABLE IF NOT EXISTS schedule_active (
			user_id INTEGER NOT NULL, chat_id INTEGER NOT NULL,
			revision INTEGER NOT NULL, data TEXT NOT NULL,
			PRIMARY KEY(user_id, chat_id)
		)`,
		`CREATE TABLE IF NOT EXISTS schedule_notifications (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL, chat_id INTEGER NOT NULL, revision INTEGER NOT NULL,
			occurrence_key TEXT NOT NULL, start_at INTEGER NOT NULL, trigger_at INTEGER NOT NULL,
			local_date TEXT NOT NULL, local_start TEXT NOT NULL,
			lesson TEXT NOT NULL, timezone TEXT NOT NULL, lead_minutes INTEGER NOT NULL,
			status INTEGER NOT NULL DEFAULT 0,
			UNIQUE(user_id, chat_id, occurrence_key)
		)`,
		`CREATE INDEX IF NOT EXISTS schedule_notifications_due ON schedule_notifications(status, trigger_at)`,
		`CREATE TABLE IF NOT EXISTS schedule_consumed (
			user_id INTEGER NOT NULL, chat_id INTEGER NOT NULL, occurrence_key TEXT NOT NULL,
			start_at INTEGER NOT NULL, consumed_at INTEGER NOT NULL,
			PRIMARY KEY(user_id, chat_id, occurrence_key)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize schedule database: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func validOwner(userID, chatID int64) error {
	if userID <= 0 || chatID <= 0 {
		return errors.New("schedules require a positive user and private chat ID")
	}
	return nil
}

func marshalDraft(d Draft) ([]byte, error) {
	if len(d.Inputs) > 12 {
		return nil, errors.New("at most 12 schedule inputs are allowed")
	}
	notes, err := json.Marshal(d.Notes)
	if err != nil {
		return nil, err
	}
	if len(notes) > 16*1024 {
		return nil, errors.New("schedule clarification notes are too large")
	}
	data, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("schedule draft is too large")
	}
	return data, nil
}

func (s *Store) SaveDraft(ctx context.Context, d Draft) (Draft, error) {
	if err := validOwner(d.UserID, d.ChatID); err != nil {
		return Draft{}, err
	}
	if _, err := marshalDraft(d); err != nil {
		return Draft{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback()
	if d.Revision > 0 {
		var current int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM schedule_drafts WHERE user_id = ? AND chat_id = ?`, d.UserID, d.ChatID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current != d.Revision) {
			return Draft{}, ErrDraftChanged
		}
		if err != nil {
			return Draft{}, err
		}
	}
	// Keep this counter outside draft rows: deleting a draft must never make an
	// old Telegram confirmation button valid for a newly uploaded schedule.
	err = tx.QueryRowContext(ctx, `UPDATE schedule_metadata SET value = value + 1 WHERE name = 'revision' RETURNING value`).Scan(&d.Revision)
	if err != nil {
		return Draft{}, err
	}
	data, err := marshalDraft(d)
	if err != nil {
		return Draft{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO schedule_drafts(user_id, chat_id, revision, data) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, chat_id) DO UPDATE SET revision = excluded.revision, data = excluded.data`,
		d.UserID, d.ChatID, d.Revision, string(data))
	if err != nil {
		return Draft{}, err
	}
	if err := tx.Commit(); err != nil {
		return Draft{}, err
	}
	return d, nil
}

func (s *Store) GetDraft(ctx context.Context, userID, chatID int64) (Draft, error) {
	var data string
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM schedule_drafts WHERE user_id = ? AND chat_id = ?`, userID, chatID).Scan(&data); err != nil {
		return Draft{}, err
	}
	var d Draft
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return Draft{}, fmt.Errorf("decode schedule draft: %w", err)
	}
	return d, nil
}

func (s *Store) DeleteDraft(ctx context.Context, userID, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedule_drafts WHERE user_id = ? AND chat_id = ?`, userID, chatID)
	return err
}

func (s *Store) GetActive(ctx context.Context, userID, chatID int64) (Active, error) {
	var data string
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM schedule_active WHERE user_id = ? AND chat_id = ?`, userID, chatID).Scan(&data); err != nil {
		return Active{}, err
	}
	var a Active
	if err := json.Unmarshal([]byte(data), &a); err != nil {
		return Active{}, fmt.Errorf("decode active schedule: %w", err)
	}
	return a, nil
}

func (s *Store) Activate(ctx context.Context, userID, chatID, expectedRevision int64, lead int, now time.Time) (Active, int, error) {
	if err := validOwner(userID, chatID); err != nil {
		return Active{}, 0, err
	}
	if lead != 10 && lead != 30 && lead != 60 {
		return Active{}, 0, errors.New("schedule reminder lead must be 10, 30 or 60 minutes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Active{}, 0, err
	}
	defer tx.Rollback()
	var data string
	err = tx.QueryRowContext(ctx, `SELECT data FROM schedule_drafts WHERE user_id = ? AND chat_id = ? AND revision = ?`, userID, chatID, expectedRevision).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Active{}, 0, ErrDraftChanged
	}
	if err != nil {
		return Active{}, 0, err
	}
	var d Draft
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return Active{}, 0, err
	}
	d.Plan.LeadMinutes = lead
	plan, err := Normalize(d.Plan)
	if err != nil {
		return Active{}, 0, err
	}
	occurrences, err := Generate(plan, now)
	if err != nil {
		return Active{}, 0, err
	}
	if len(occurrences) == 0 {
		return Active{}, 0, errors.New("schedule has no future reminder times")
	}
	location, err := time.LoadLocation(plan.Timezone)
	if err != nil {
		return Active{}, 0, err
	}
	a := Active{UserID: userID, ChatID: chatID, Revision: expectedRevision, Plan: plan}
	activeData, err := json.Marshal(a)
	if err != nil {
		return Active{}, 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO schedule_active(user_id, chat_id, revision, data) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, chat_id) DO UPDATE SET revision = excluded.revision, data = excluded.data`,
		userID, chatID, expectedRevision, string(activeData))
	if err != nil {
		return Active{}, 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_notifications WHERE user_id = ? AND chat_id = ?`, userID, chatID); err != nil {
		return Active{}, 0, err
	}
	count := 0
	for _, occurrence := range occurrences {
		lessonData, err := json.Marshal(occurrence.Lesson)
		if err != nil {
			return Active{}, 0, err
		}
		localStart := occurrence.StartAt.In(location)
		result, err := tx.ExecContext(ctx, `INSERT INTO schedule_notifications(
			user_id, chat_id, revision, occurrence_key, start_at, trigger_at,
			local_date, local_start, lesson, timezone, lead_minutes)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (SELECT 1 FROM schedule_consumed WHERE user_id = ? AND chat_id = ? AND occurrence_key = ?)
			ON CONFLICT(user_id, chat_id, occurrence_key) DO NOTHING`,
			userID, chatID, expectedRevision, occurrence.Key, occurrence.StartAt.Unix(), occurrence.TriggerAt.Unix(),
			localStart.Format("2006-01-02"), localStart.Format("15:04"), string(lessonData), plan.Timezone, lead,
			userID, chatID, occurrence.Key)
		if err != nil {
			return Active{}, 0, err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return Active{}, 0, err
		}
		count += int(inserted)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_drafts WHERE user_id = ? AND chat_id = ? AND revision = ?`, userID, chatID, expectedRevision); err != nil {
		return Active{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return Active{}, 0, err
	}
	return a, count, nil
}

func (s *Store) Disable(ctx context.Context, userID, chatID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_active WHERE user_id = ? AND chat_id = ?`, userID, chatID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schedule_notifications WHERE user_id = ? AND chat_id = ?`, userID, chatID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Skip(ctx context.Context, userID, chatID int64, date, start string) (int, error) {
	if parsed, err := time.Parse("2006-01-02", date); err != nil || parsed.Format("2006-01-02") != date {
		return 0, errors.New("invalid date for skipped class")
	}
	if parsed, err := time.Parse("15:04", start); err != nil || parsed.Format("15:04") != start {
		return 0, errors.New("invalid start time for skipped class")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// The exclusion shares the durable occurrence identity with delivery, so a
	// corrected or reimported plan cannot resurrect a deliberately canceled class.
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO schedule_consumed(user_id, chat_id, occurrence_key, start_at, consumed_at)
		SELECT user_id, chat_id, occurrence_key, start_at, ? FROM schedule_notifications
		WHERE user_id = ? AND chat_id = ? AND local_date = ? AND local_start = ? AND status IN (0, 1)
		AND EXISTS (SELECT 1 FROM schedule_active a WHERE a.user_id = schedule_notifications.user_id
			AND a.chat_id = schedule_notifications.chat_id AND a.revision = schedule_notifications.revision)`, time.Now().Unix(), userID, chatID, date, start)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE schedule_notifications SET status = 3
		WHERE user_id = ? AND chat_id = ? AND local_date = ? AND local_start = ? AND status IN (0, 1)
		AND EXISTS (SELECT 1 FROM schedule_active a WHERE a.user_id = schedule_notifications.user_id
			AND a.chat_id = schedule_notifications.chat_id AND a.revision = schedule_notifications.revision)`, userID, chatID, date, start)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(count), nil
}

// CanDeliver rechecks a claimed notification immediately before sending, so a
// queued item canceled by skip/off or replaced by another plan is discarded.
func (s *Store) CanDeliver(ctx context.Context, n Notification) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM schedule_notifications n JOIN schedule_active a
		ON a.user_id = n.user_id AND a.chat_id = n.chat_id AND a.revision = n.revision
		WHERE n.id = ? AND n.user_id = ? AND n.chat_id = ? AND n.revision = ? AND n.status = 1
	)`, n.ID, n.UserID, n.ChatID, n.Revision).Scan(&allowed)
	return allowed, err
}

// TakeDue consumes notifications before returning them. Telegram sends cannot be
// part of a SQLite transaction, so an uncertain send is deliberately not retried:
// a crash may lose one notification, but must not send it twice after restarting.
func (s *Store) TakeDue(ctx context.Context, now time.Time, limit int) ([]Notification, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > 100 {
		limit = 100
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cutoff := now.Add(-5 * time.Minute).Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE schedule_notifications SET status = 2 WHERE status = 0 AND (trigger_at < ? OR start_at <= ?)`, cutoff, now.Unix()); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT n.id, n.user_id, n.chat_id, n.revision, n.start_at, n.trigger_at, n.lesson, n.timezone, n.lead_minutes, n.occurrence_key
		FROM schedule_notifications n JOIN schedule_active a ON a.user_id = n.user_id AND a.chat_id = n.chat_id AND a.revision = n.revision
		WHERE n.status = 0 AND n.trigger_at <= ? AND n.trigger_at >= ? AND n.start_at > ?
		ORDER BY n.trigger_at, n.id LIMIT ?`, now.Unix(), cutoff, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		n   Notification
		key string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var start, trigger int64
		var lessonData string
		if err := rows.Scan(&c.n.ID, &c.n.UserID, &c.n.ChatID, &c.n.Revision, &start, &trigger, &lessonData, &c.n.Timezone, &c.n.LeadMinutes, &c.key); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(lessonData), &c.n.Lesson); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.n.StartAt, c.n.TriggerAt = time.Unix(start, 0).UTC(), time.Unix(trigger, 0).UTC()
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var result []Notification
	for _, c := range candidates {
		insert, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schedule_consumed(user_id, chat_id, occurrence_key, start_at, consumed_at) VALUES (?, ?, ?, ?, ?)`,
			c.n.UserID, c.n.ChatID, c.key, c.n.StartAt.Unix(), now.Unix())
		if err != nil {
			return nil, err
		}
		inserted, err := insert.RowsAffected()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schedule_notifications SET status = 1 WHERE id = ? AND status = 0`, c.n.ID); err != nil {
			return nil, err
		}
		if inserted != 0 {
			result = append(result, c.n)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
