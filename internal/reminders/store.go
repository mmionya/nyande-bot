package reminders

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Reminder struct {
	ID        int64     `json:"id"`
	ChatID    int64     `json:"chat_id"`
	UserID    int64     `json:"user_id"`
	UserName  string    `json:"user_name"`
	MessageID int       `json:"message_id"`
	Text      string    `json:"text"`
	TriggerAt time.Time `json:"trigger_at"`
	CreatedAt time.Time `json:"created_at"`
	Completed bool      `json:"completed"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("reminders database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create reminders directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open reminders database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize() error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS reminders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			user_name TEXT NOT NULL DEFAULT '',
			message_id INTEGER NOT NULL DEFAULT 0,
			text TEXT NOT NULL,
			trigger_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			completed INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(completed, trigger_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize reminders database: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Save(ctx context.Context, r Reminder) (int64, error) {
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return 0, errors.New("reminder text cannot be empty")
	}
	triggerUnix := r.TriggerAt.UTC().Unix()
	createdUnix := r.CreatedAt.UTC().Unix()
	if createdUnix <= 0 {
		createdUnix = time.Now().UTC().Unix()
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO reminders(chat_id, user_id, user_name, message_id, text, trigger_at, created_at, completed)
		VALUES(?, ?, ?, ?, ?, ?, ?, 0)
	`, r.ChatID, r.UserID, r.UserName, r.MessageID, text, triggerUnix, createdUnix)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) GetDue(ctx context.Context, now time.Time) ([]Reminder, error) {
	nowUnix := now.UTC().Unix()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, user_id, user_name, message_id, text, trigger_at, created_at, completed
		FROM reminders
		WHERE completed = 0 AND trigger_at <= ?
		ORDER BY trigger_at ASC
		LIMIT 50
	`, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Reminder
	for rows.Next() {
		var (
			r           Reminder
			triggerUnix int64
			createdUnix int64
			compInt     int
		)
		if err := rows.Scan(
			&r.ID, &r.ChatID, &r.UserID, &r.UserName, &r.MessageID,
			&r.Text, &triggerUnix, &createdUnix, &compInt,
		); err != nil {
			return nil, err
		}
		r.TriggerAt = time.Unix(triggerUnix, 0).UTC()
		r.CreatedAt = time.Unix(createdUnix, 0).UTC()
		r.Completed = compInt != 0
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) MarkCompleted(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE reminders SET completed = 1 WHERE id = ?`, id)
	return err
}
