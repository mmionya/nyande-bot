package quotes

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const MaxTextRunes = 1200

var ErrInvalidTrigger = errors.New("quote trigger must be one word of at most 32 letters, numbers, underscores or hyphens")

type Quote struct {
	ID, ChatID, AuthorID, SavedBy, Date int64
	MessageID                           int
	Author, Text                        string
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("quote database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
		CREATE TABLE IF NOT EXISTS quotes (
		id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL, author_id INTEGER NOT NULL, saved_by INTEGER NOT NULL,
		author TEXT NOT NULL, text TEXT NOT NULL, date INTEGER NOT NULL,
		UNIQUE(chat_id, message_id));
		CREATE TABLE IF NOT EXISTS quote_settings (
		chat_id INTEGER PRIMARY KEY, trigger_word TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Trigger(ctx context.Context, chatID int64) (string, error) {
	var trigger string
	err := s.db.QueryRowContext(ctx, `SELECT trigger_word FROM quote_settings WHERE chat_id=?`, chatID).Scan(&trigger)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return trigger, err
}

func (s *Store) SetTrigger(ctx context.Context, chatID int64, trigger string) error {
	if !utf8.ValidString(trigger) {
		return ErrInvalidTrigger
	}
	trigger = strings.ToLower(strings.TrimSpace(trigger))
	if utf8.RuneCountInString(trigger) > 32 || strings.IndexFunc(trigger, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-'
	}) >= 0 {
		return ErrInvalidTrigger
	}
	if trigger == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM quote_settings WHERE chat_id=?`, chatID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO quote_settings (chat_id, trigger_word) VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET trigger_word=excluded.trigger_word`, chatID, trigger)
	return err
}

const columns = `id, chat_id, message_id, author_id, saved_by, author, text, date`

func scan(row interface{ Scan(...any) error }) (Quote, error) {
	var q Quote
	err := row.Scan(&q.ID, &q.ChatID, &q.MessageID, &q.AuthorID, &q.SavedBy, &q.Author, &q.Text, &q.Date)
	return q, err
}

func (s *Store) Add(ctx context.Context, q Quote) (Quote, error) {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" || utf8.RuneCountInString(q.Text) > MaxTextRunes {
		return Quote{}, errors.New("quote must contain 1–1200 characters")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO quotes (chat_id, message_id, author_id, saved_by, author, text, date)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(chat_id, message_id) DO NOTHING`,
		q.ChatID, q.MessageID, q.AuthorID, q.SavedBy, q.Author, q.Text, q.Date)
	if err != nil {
		return Quote{}, err
	}
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM quotes WHERE chat_id=? AND message_id=?`, q.ChatID, q.MessageID))
}

func (s *Store) Get(ctx context.Context, chatID, id int64) (Quote, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM quotes WHERE chat_id=? AND id=?`, chatID, id))
}

func (s *Store) Random(ctx context.Context, chatID int64) (Quote, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM quotes WHERE chat_id=? ORDER BY RANDOM() LIMIT 1`, chatID))
}

func (s *Store) List(ctx context.Context, chatID int64) ([]Quote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM quotes WHERE chat_id=? ORDER BY id DESC LIMIT 10`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Quote
	for rows.Next() {
		q, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, q)
	}
	return result, rows.Err()
}

func (s *Store) Delete(ctx context.Context, chatID, id, userID int64, admin bool) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM quotes WHERE chat_id=? AND id=? AND (? OR (?>0 AND (author_id=? OR saved_by=?)))`, chatID, id, admin, userID, userID, userID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
