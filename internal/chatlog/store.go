package chatlog

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
	"unicode"

	"github.com/mmionya/nyande-bot/internal/storagecrypt"
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

type messageBody struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Text        string `json:"text"`
}

type Store struct {
	db         *sql.DB
	maxPerChat int
	cipher     *storagecrypt.Cipher
}

const messageSchema = `(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 chat_id INTEGER NOT NULL,
 message_id INTEGER NOT NULL,
 date INTEGER NOT NULL,
 payload BLOB NOT NULL,
 UNIQUE(chat_id, message_id)
)`
const messageColumns = `id, chat_id, message_id, date, payload`
const keyCheck = "nyande-chatlog-aes-gcm-v1"

func Open(path string, maxPerChat int, key []byte) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("chat log database path is empty")
	}
	cipher, err := storagecrypt.New(key)
	if err != nil {
		return nil, err
	}
	if maxPerChat < 1 {
		maxPerChat = 50000
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create chat log directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open chat log file: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open chat log database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, maxPerChat: maxPerChat, cipher: cipher}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize() error {
	for _, statement := range []string{
		`PRAGMA busy_timeout = 5000`, `PRAGMA secure_delete = ON`,
		`PRAGMA temp_store = MEMORY`, `PRAGMA journal_mode = WAL`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize chat log database: %w", err)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS chatlog_metadata (name TEXT PRIMARY KEY, value BLOB NOT NULL)`); err != nil {
		return err
	}
	var mode string
	err = tx.QueryRow(`SELECT value FROM chatlog_metadata WHERE name = 'format'`).Scan(&mode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var exists, hasPayload bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'messages')`).Scan(&exists); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('messages') WHERE name = 'payload')`).Scan(&hasPayload); err != nil {
		return err
	}
	switch mode {
	case "aes-gcm-v1":
		if s.cipher == nil {
			return errors.New("chat log encryption key is required")
		}
		var sealed []byte
		if err := tx.QueryRow(`SELECT value FROM chatlog_metadata WHERE name = 'key_check'`).Scan(&sealed); err != nil {
			return errors.New("chat log encryption key check is missing")
		}
		plain, err := s.cipher.Open(sealed, []byte(keyCheck))
		if err != nil || string(plain) != keyCheck {
			return errors.New("chat log encryption key is incorrect or archive is damaged")
		}
		if !hasPayload {
			return errors.New("encrypted chat log schema is damaged")
		}
	case "plain-v1":
		if !hasPayload {
			return errors.New("chat log schema is damaged")
		}
	case "":
		if hasPayload {
			return errors.New("chat log format marker is missing")
		}
	default:
		return errors.New("unsupported chat log format")
	}
	migrate := exists && (!hasPayload || mode == "plain-v1" && s.cipher != nil)
	if migrate {
		if err := s.migrate(tx, hasPayload); err != nil {
			return fmt.Errorf("migrate chat log: %w", err)
		}
	} else if !exists {
		if _, err := tx.Exec(`CREATE TABLE messages ` + messageSchema); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_chat_date ON messages(chat_id, date DESC, message_id DESC)`); err != nil {
		return err
	}
	if mode == "" || migrate {
		mode = "plain-v1"
		if s.cipher != nil {
			mode = "aes-gcm-v1"
			if _, err := tx.Exec(`INSERT OR REPLACE INTO chatlog_metadata(name, value) VALUES ('key_check', ?)`, s.cipher.Seal([]byte(keyCheck), []byte(keyCheck))); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO chatlog_metadata(name, value) VALUES ('format', ?)`, mode); err != nil {
			return err
		}
	}
	if migrate && s.cipher != nil {
		// Resume cleanup after a crash between the committed migration and VACUUM.
		if _, err := tx.Exec(`INSERT OR REPLACE INTO chatlog_metadata(name, value) VALUES ('cleanup_pending', '1')`); err != nil {
			return err
		}
	}
	var cleanup bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM chatlog_metadata WHERE name = 'cleanup_pending')`).Scan(&cleanup); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if cleanup {
		if err := s.checkpoint(); err != nil {
			return err
		}
		if _, err := s.db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("compact encrypted chat log: %w", err)
		}
		if err := s.checkpoint(); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM chatlog_metadata WHERE name = 'cleanup_pending'`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) checkpoint() error {
	var busy, pages, checkpointed int
	if err := s.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &pages, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("chat log cleanup is blocked by another database connection")
	}
	return nil
}

func (s *Store) migrate(tx *sql.Tx, hasPayload bool) error {
	if _, err := tx.Exec(`CREATE TABLE messages_migration ` + messageSchema); err != nil {
		return err
	}
	columns := `id, chat_id, message_id, user_id, username, display_name, text, date`
	if hasPayload {
		columns = messageColumns
	}
	rows, err := tx.Query(`SELECT ` + columns + ` FROM messages`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var msg Message
		var date int64
		if hasPayload {
			var plain []byte
			if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.MessageID, &date, &plain); err != nil {
				return err
			}
			if err := json.Unmarshal(plain, &msg); err != nil {
				return errors.New("invalid plaintext chat log record")
			}
		} else if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.MessageID, &msg.UserID, &msg.Username, &msg.DisplayName, &msg.Text, &date); err != nil {
			return err
		}
		msg.Date = time.Unix(date, 0).UTC()
		payload, err := s.encode(msg)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO messages_migration(id, chat_id, message_id, date, payload) VALUES (?, ?, ?, ?, ?)`, msg.ID, msg.ChatID, msg.MessageID, date, payload); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS messages_ai`, `DROP TRIGGER IF EXISTS messages_ad`, `DROP TRIGGER IF EXISTS messages_au`,
		`DROP TABLE IF EXISTS messages_fts`, `DROP TABLE messages`, `ALTER TABLE messages_migration RENAME TO messages`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
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

func messageAAD(msg Message) []byte {
	return []byte(fmt.Sprintf("nyande-chatlog-message-v1:%d:%d:%d", msg.ChatID, msg.MessageID, msg.Date.Unix()))
}

func (s *Store) encode(msg Message) ([]byte, error) {
	data, err := json.Marshal(messageBody{msg.UserID, msg.Username, msg.DisplayName, msg.Text})
	if err != nil {
		return nil, err
	}
	if s.cipher != nil {
		data = s.cipher.Seal(data, messageAAD(msg))
	}
	return data, nil
}

func (s *Store) scan(rows *sql.Rows) (Message, error) {
	var msg Message
	var date int64
	var payload []byte
	if err := rows.Scan(&msg.ID, &msg.ChatID, &msg.MessageID, &date, &payload); err != nil {
		return msg, err
	}
	msg.Date = time.Unix(date, 0).UTC()
	if s.cipher != nil {
		var err error
		payload, err = s.cipher.Open(payload, messageAAD(msg))
		if err != nil {
			return Message{}, errors.New("chat log record authentication failed")
		}
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return Message{}, errors.New("invalid chat log record")
	}
	return msg, nil
}

func (s *Store) Save(ctx context.Context, msg Message) error {
	msg.Text = strings.TrimSpace(msg.Text)
	if msg.Text == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE chat_id = ? AND message_id = ?`, msg.ChatID, msg.MessageID)
		return err
	}
	date := msg.Date.UTC().Unix()
	if date <= 0 {
		date = time.Now().UTC().Unix()
	}
	msg.Date = time.Unix(date, 0).UTC()
	payload, err := s.encode(msg)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
  INSERT INTO messages(chat_id, message_id, date, payload) VALUES (?, ?, ?, ?)
  ON CONFLICT(chat_id, message_id) DO UPDATE SET payload = excluded.payload, date = excluded.date
 `, msg.ChatID, msg.MessageID, date, payload)
	if err != nil {
		return err
	}
	return s.prune(ctx, msg.ChatID)
}

func (s *Store) prune(ctx context.Context, chatID int64) error {
	// ponytail: O(n) per save (normally 50k retained rows); use persisted per-chat
	// counters if message rates outgrow this indexed scan.
	_, err := s.db.ExecContext(ctx, `
  DELETE FROM messages WHERE id IN (
   SELECT id FROM messages WHERE chat_id = ? ORDER BY date DESC, message_id DESC LIMIT -1 OFFSET ?
  )`, chatID, s.maxPerChat)
	return err
}

func (s *Store) Search(ctx context.Context, chatID int64, query string, limit int) ([]Message, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, nil
	}
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	terms := strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	// ponytail: O(n) within one chat (normally 50k retained messages), newest first.
	// A keyed search index is the upgrade path if this bounded scan becomes slow.
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE chat_id = ? ORDER BY date DESC, message_id DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Message
	for rows.Next() {
		msg, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		text := strings.ToLower(msg.Text)
		match := strings.Contains(text, query)
		if !match && len(terms) > 0 {
			match = true
			for _, term := range terms {
				if !strings.Contains(text, term) {
					match = false
					break
				}
			}
		}
		if match {
			result = append(result, msg)
		}
		if len(result) == limit {
			break
		}
	}
	return result, rows.Err()
}

func (s *Store) Recent(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE chat_id = ? ORDER BY date DESC, message_id DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		msg, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}
