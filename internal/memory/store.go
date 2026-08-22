package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const MaxContentRunes = 500

var (
	ErrEmpty     = errors.New("memory is empty")
	ErrTooLong   = errors.New("memory is too long")
	ErrSensitive = errors.New("memory appears to contain sensitive data")

	telegramTokenPattern = regexp.MustCompile(`\b[0-9]{8,12}:[A-Za-z0-9_-]{30,}\b`)
	secretTokenPattern   = regexp.MustCompile(`(?i)\b(?:sk|rk|pk)_[a-z0-9_-]{16,}\b|\bsk-[a-z0-9_-]{16,}\b`)
	passwordPattern      = regexp.MustCompile(`(?i)(?:password|passwd|парол[ьяюе]?|токен|secret|api[_ -]?key)\s*(?:is|это|=|:|-)?\s*\S+`)
)

type Memory struct {
	ID        int64
	ChatID    int64
	UserID    int64
	UserName  string
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Store struct {
	db          *sql.DB
	maxPerScope int
}

func Open(path string, maxPerScope int) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("memory database path is empty")
	}
	if maxPerScope < 1 {
		maxPerScope = 100
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create memory directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open memory database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, maxPerScope: maxPerScope}
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
		`CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			user_name TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL COLLATE NOCASE,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(chat_id, user_id, content)
		)`,
		`CREATE INDEX IF NOT EXISTS memories_scope_updated
			ON memories(chat_id, user_id, updated_at DESC)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			content,
			content='memories',
			content_rowid='id',
			tokenize='unicode61 remove_diacritics 2'
		)`,
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.id, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE OF content ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.id, old.content);
			INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
		END`,
		`INSERT INTO memories_fts(memories_fts) VALUES ('rebuild')`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize memory database: %w", err)
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

func (s *Store) Remember(ctx context.Context, chatID, userID int64, userName, content string) (Memory, bool, error) {
	content = normalize(content)
	if content == "" {
		return Memory{}, false, ErrEmpty
	}
	if utf8.RuneCountInString(content) > MaxContentRunes {
		return Memory{}, false, ErrTooLong
	}
	if sensitive(content) {
		return Memory{}, false, ErrSensitive
	}
	now := time.Now().UTC().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Memory{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO memories(chat_id, user_id, user_name, content, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, chatID, userID, strings.TrimSpace(userName), content, now, now)
	if err != nil {
		return Memory{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Memory{}, false, err
	}
	created := rows > 0
	if !created {
		if _, err := tx.ExecContext(ctx, `
			UPDATE memories SET user_name = ?, updated_at = ?
			WHERE chat_id = ? AND user_id = ? AND content = ?`,
			strings.TrimSpace(userName), now, chatID, userID, content); err != nil {
			return Memory{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM memories
		WHERE id IN (
			SELECT id FROM memories
			WHERE chat_id = ? AND user_id = ?
			ORDER BY updated_at DESC, id DESC
			LIMIT -1 OFFSET ?
		)`, chatID, userID, s.maxPerScope); err != nil {
		return Memory{}, false, err
	}
	memory, err := scanOne(tx.QueryRowContext(ctx, `
		SELECT id, chat_id, user_id, user_name, content, created_at, updated_at
		FROM memories WHERE chat_id = ? AND user_id = ? AND content = ?`, chatID, userID, content))
	if err != nil {
		return Memory{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Memory{}, false, err
	}
	return memory, created, nil
}

func (s *Store) Recall(ctx context.Context, chatID, userID int64, query string, limit int) ([]Memory, error) {
	if limit < 1 {
		return nil, nil
	}
	seen := make(map[int64]bool)
	result := make([]Memory, 0, limit)
	if expression := ftsExpression(query); expression != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT m.id, m.chat_id, m.user_id, m.user_name, m.content, m.created_at, m.updated_at
			FROM memories_fts AS f
			JOIN memories AS m ON m.id = f.rowid
			WHERE memories_fts MATCH ? AND m.chat_id = ? AND m.user_id = ?
			ORDER BY bm25(memories_fts), m.updated_at DESC
			LIMIT ?`, expression, chatID, userID, limit)
		if err != nil {
			return nil, err
		}
		result, err = scanRows(rows, result, seen)
		if err != nil {
			return nil, err
		}
	}
	if len(result) < limit {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, chat_id, user_id, user_name, content, created_at, updated_at
			FROM memories WHERE chat_id = ? AND user_id = ?
			ORDER BY updated_at DESC, id DESC LIMIT ?`, chatID, userID, limit)
		if err != nil {
			return nil, err
		}
		result, err = scanRows(rows, result, seen)
		if err != nil {
			return nil, err
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *Store) List(ctx context.Context, chatID, userID int64, limit int) ([]Memory, error) {
	if limit < 1 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, chat_id, user_id, user_name, content, created_at, updated_at
		FROM memories WHERE chat_id = ? AND user_id = ?
		ORDER BY updated_at DESC, id DESC LIMIT ?`, chatID, userID, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, nil, nil)
}

func (s *Store) Forget(ctx context.Context, chatID, userID, memoryID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM memories WHERE id = ? AND chat_id = ? AND user_id = ?`, memoryID, chatID, userID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (s *Store) ForgetAll(ctx context.Context, chatID, userID int64) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM memories WHERE chat_id = ? AND user_id = ?`, chatID, userID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type rowScanner interface {
	Scan(...any) error
}

func scanOne(row rowScanner) (Memory, error) {
	var item Memory
	var createdAt, updatedAt int64
	err := row.Scan(&item.ID, &item.ChatID, &item.UserID, &item.UserName, &item.Content, &createdAt, &updatedAt)
	item.CreatedAt = time.Unix(createdAt, 0).UTC()
	item.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return item, err
}

func scanRows(rows *sql.Rows, target []Memory, seen map[int64]bool) ([]Memory, error) {
	defer rows.Close()
	for rows.Next() {
		item, err := scanOne(rows)
		if err != nil {
			return nil, err
		}
		if seen != nil {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
		}
		target = append(target, item)
	}
	return target, rows.Err()
}

func normalize(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func sensitive(value string) bool {
	return strings.Contains(value, "-----BEGIN PRIVATE KEY-----") ||
		telegramTokenPattern.MatchString(value) || secretTokenPattern.MatchString(value) ||
		passwordPattern.MatchString(value)
}

func ftsExpression(value string) string {
	unique := make(map[string]bool)
	var tokens []string
	for _, field := range strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	}) {
		if utf8.RuneCountInString(field) < 2 || unique[field] {
			continue
		}
		unique[field] = true
		tokens = append(tokens, field)
	}
	sort.SliceStable(tokens, func(left, right int) bool {
		return utf8.RuneCountInString(tokens[left]) > utf8.RuneCountInString(tokens[right])
	})
	if len(tokens) > 8 {
		tokens = tokens[:8]
	}
	for index, token := range tokens {
		tokens[index] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"*`
	}
	return strings.Join(tokens, " OR ")
}
