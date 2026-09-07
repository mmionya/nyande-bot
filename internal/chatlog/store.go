package chatlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

type Message struct {
	ID          int64     `json:"id"`
	ChatID      int64     `json:"chat_id"`
	MessageID   int       `json:"message_id"`
	UserID      int64     `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Text        string    `json:"text"`
	Date        time.Time `json:"date"`
}

type Store struct {
	db         *sql.DB
	maxPerChat int
}

func Open(path string, maxPerChat int) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("chat log database path is empty")
	}
	if maxPerChat < 1 {
		maxPerChat = 50000
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create chat log directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open chat log database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, maxPerChat: maxPerChat}
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
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL,
			date INTEGER NOT NULL,
			UNIQUE(chat_id, message_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_chat_date
			ON messages(chat_id, date DESC)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			text,
			content='messages',
			content_rowid='id',
			tokenize='unicode61 remove_diacritics 2'
		)`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, text) VALUES (new.id, new.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.id, old.text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE OF text ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text) VALUES ('delete', old.id, old.text);
			INSERT INTO messages_fts(rowid, text) VALUES (new.id, new.text);
		END`,
		`INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize chat log database: %w", err)
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

func (s *Store) Save(ctx context.Context, msg Message) error {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return nil
	}
	dateUnix := msg.Date.UTC().Unix()
	if dateUnix <= 0 {
		dateUnix = time.Now().UTC().Unix()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages(chat_id, message_id, user_id, username, display_name, text, date)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, message_id) DO UPDATE SET
			username = excluded.username,
			display_name = excluded.display_name,
			text = excluded.text,
			date = excluded.date
	`, msg.ChatID, msg.MessageID, msg.UserID, msg.Username, msg.DisplayName, text, dateUnix)
	if err != nil {
		return err
	}

	// Prune periodically when message ID is a multiple of 100
	if s.maxPerChat > 0 && msg.MessageID%100 == 0 {
		_ = s.prune(ctx, msg.ChatID)
	}
	return nil
}

func (s *Store) prune(ctx context.Context, chatID int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM messages
		WHERE chat_id = ? AND id NOT IN (
			SELECT id FROM messages
			WHERE chat_id = ?
			ORDER BY date DESC, message_id DESC
			LIMIT ?
		)
	`, chatID, chatID, s.maxPerChat)
	return err
}

func (s *Store) Search(ctx context.Context, chatID int64, query string, limit int) ([]Message, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	result := make([]Message, 0, limit)
	seen := make(map[int]bool)

	// Try FTS5 first
	ftsQuery := buildFTSQuery(query)
	if ftsQuery != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT m.id, m.chat_id, m.message_id, m.user_id, m.username, m.display_name, m.text, m.date
			FROM messages_fts AS f
			JOIN messages AS m ON m.id = f.rowid
			WHERE messages_fts MATCH ? AND m.chat_id = ?
			ORDER BY bm25(messages_fts), m.date DESC
			LIMIT ?
		`, ftsQuery, chatID, limit)
		if err == nil {
			result, _ = scanMessages(rows, result, seen)
		}
	}

	// Fallback/supplement with LIKE query if under limit
	if len(result) < limit {
		likePattern := "%" + query + "%"
		remaining := limit - len(result)
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, chat_id, message_id, user_id, username, display_name, text, date
			FROM messages
			WHERE chat_id = ? AND text LIKE ?
			ORDER BY date DESC, message_id DESC
			LIMIT ?
		`, chatID, likePattern, remaining*2)
		if err == nil {
			result, _ = scanMessages(rows, result, seen)
		}
	}

	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *Store) Recent(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, message_id, user_id, username, display_name, text, date
		FROM messages
		WHERE chat_id = ?
		ORDER BY date DESC, message_id DESC
		LIMIT ?
	`, chatID, limit)
	if err != nil {
		return nil, err
	}

	messages, err := scanMessages(rows, nil, nil)
	if err != nil {
		return nil, err
	}

	// Reverse to return in chronological order (oldest first)
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

func scanMessages(rows *sql.Rows, dest []Message, seen map[int]bool) ([]Message, error) {
	defer rows.Close()
	for rows.Next() {
		var (
			msg      Message
			dateUnix int64
		)
		if err := rows.Scan(
			&msg.ID, &msg.ChatID, &msg.MessageID, &msg.UserID,
			&msg.Username, &msg.DisplayName, &msg.Text, &dateUnix,
		); err != nil {
			return dest, err
		}
		if seen != nil {
			if seen[msg.MessageID] {
				continue
			}
			seen[msg.MessageID] = true
		}
		msg.Date = time.Unix(dateUnix, 0).UTC()
		dest = append(dest, msg)
	}
	return dest, rows.Err()
}

func buildFTSQuery(raw string) string {
	var tokens []string
	for _, field := range strings.Fields(raw) {
		clean := strings.TrimFunc(field, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if clean != "" {
			tokens = append(tokens, `"`+clean+`"`)
		}
	}
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " ")
}
