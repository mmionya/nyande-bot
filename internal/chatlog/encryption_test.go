package chatlog

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncryptedArchivePersistenceAndAuthentication(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x71}, 32)
	path := filepath.Join(t.TempDir(), "archive.db")
	store, err := Open(path, 100, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	first := Message{ChatID: -10, MessageID: 1, UserID: 12345, Username: "privateusernamecanary", DisplayName: "PrivateAuthorCanary", Text: "PrivateArchiveCanary Привет кошкам", Date: time.Unix(1700000000, 0).UTC()}
	second := first
	second.MessageID, second.Text, second.Date = 2, "SecretSecondCanary кошкам очень ПРИВЕТ", first.Date.Add(time.Second)
	other := first
	other.ChatID = -20
	for _, msg := range []Message{first, second, other} {
		if err := store.Save(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	matches, err := store.Search(ctx, -10, "привет кошкам", 10)
	if err != nil || len(matches) != 2 || matches[0].MessageID != 2 {
		t.Fatalf("word search/order: %+v, %v", matches, err)
	}
	if otherChat, err := store.Recent(ctx, -20, 50); err != nil || len(otherChat) != 1 || otherChat[0].UserID != first.UserID {
		t.Fatalf("chat isolation: %+v, %v", otherChat, err)
	}
	assertArchivePrivate(t, path, first.Text, first.Username, first.DisplayName, second.Text)
	var firstCipher []byte
	if err := store.db.QueryRow(`SELECT payload FROM messages WHERE chat_id = -10 AND message_id = 1`).Scan(&firstCipher); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	var secondCipher []byte
	if err := store.db.QueryRow(`SELECT payload FROM messages WHERE chat_id = -10 AND message_id = 1`).Scan(&secondCipher); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstCipher, secondCipher) {
		t.Fatal("repeated writes reused ciphertext/nonce")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, badKey := range [][]byte{nil, bytes.Repeat([]byte{0x72}, 32)} {
		bad, err := Open(path, 100, badKey)
		if err == nil {
			_ = bad.Close()
			t.Fatal("encrypted archive accepted missing/wrong key")
		}
	}
	store, err = Open(path, 100, key)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := store.Recent(ctx, -10, 50)
	if err != nil || len(recent) != 2 || recent[0].Text != first.Text || recent[0].Username != first.Username || recent[0].DisplayName != first.DisplayName || !recent[0].Date.Equal(first.Date) {
		t.Fatalf("restart lost fields: %+v, %v", recent, err)
	}
	first.Text = "EditedArchiveCanary new text"
	if err := store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}
	if old, err := store.Search(ctx, -10, "PrivateArchiveCanary", 10); err != nil || len(old) != 0 {
		t.Fatalf("old edited content: %+v, %v", old, err)
	}
	if edited, err := store.Search(ctx, -10, "EditedArchiveCanary", 10); err != nil || len(edited) != 1 {
		t.Fatalf("edited content missing: %+v, %v", edited, err)
	}
	// Copying valid ciphertext to another message must fail, not expose its plaintext.
	if _, err := store.db.Exec(`UPDATE messages SET payload = ? WHERE chat_id = -10 AND message_id = 2`, secondCipher); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recent(ctx, -10, 50); err == nil {
		t.Fatal("swapped record passed authentication")
	}
	if _, err := store.Search(ctx, -10, "absent", 10); err == nil {
		t.Fatal("search swallowed authentication failure")
	}
	if err := store.Save(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE messages SET date = date + 1 WHERE chat_id = -10 AND message_id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recent(ctx, -10, 50); err == nil {
		t.Fatal("changed record metadata passed authentication")
	}
	if err := store.Save(ctx, second); err != nil {
		t.Fatal(err)
	}
	secondCipher[len(secondCipher)-1] ^= 1
	if _, err := store.db.Exec(`UPDATE messages SET payload = ? WHERE chat_id = -10 AND message_id = 1`, secondCipher); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recent(ctx, -10, 50); err == nil || strings.Contains(err.Error(), first.Text) {
		t.Fatal("tampering accepted or error disclosed contents")
	}
}

func TestEmptyEncryptedArchiveRejectsWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	store, err := Open(path, 100, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]byte{nil, bytes.Repeat([]byte{2}, 32)} {
		if wrong, err := Open(path, 100, key); err == nil {
			_ = wrong.Close()
			t.Fatal("empty archive accepted wrong key")
		}
	}
}

func TestEncryptExistingChatArchive(t *testing.T) {
	for _, legacyFTS := range []bool{true, false} {
		name := "payload"
		if legacyFTS {
			name = "legacy_fts"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "migration.db")
			msg := Message{ChatID: -10, MessageID: 7, UserID: 98765, Username: "legacyusernamecanary", DisplayName: "LegacyAuthorCanary", Text: "archivelegacycanary Edited", Date: time.Unix(1700000000, 0).UTC()}
			if legacyFTS {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				for _, stmt := range []string{
					`PRAGMA journal_mode = WAL`,
					`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, user_id INTEGER NOT NULL, username TEXT NOT NULL, display_name TEXT NOT NULL, text TEXT NOT NULL, date INTEGER NOT NULL, UNIQUE(chat_id, message_id))`,
					`CREATE INDEX idx_messages_chat_date ON messages(chat_id, date DESC)`,
					`CREATE VIRTUAL TABLE messages_fts USING fts5(text, content='messages', content_rowid='id')`,
					`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN INSERT INTO messages_fts(rowid,text) VALUES (new.id,new.text); END`,
					`CREATE TRIGGER messages_au AFTER UPDATE OF text ON messages BEGIN INSERT INTO messages_fts(messages_fts,rowid,text) VALUES ('delete',old.id,old.text); INSERT INTO messages_fts(rowid,text) VALUES (new.id,new.text); END`,
				} {
					if _, err := db.Exec(stmt); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := db.Exec(`INSERT INTO messages VALUES (42,?,?,?,?,?,?,?)`, msg.ChatID, msg.MessageID, msg.UserID, msg.Username, msg.DisplayName, "erasedwordcanary", msg.Date.Unix()); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE messages SET text = ? WHERE id = 42`, msg.Text); err != nil {
					t.Fatal(err)
				}
				// Keep the original WAL present to verify migration removes its plaintext too.
			} else {
				plain, err := Open(path, 100, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := plain.Save(context.Background(), msg); err != nil {
					t.Fatal(err)
				}
				if err := plain.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			key := bytes.Repeat([]byte{3}, 32)
			encrypted, err := Open(path, 100, key)
			if err != nil {
				t.Fatal(err)
			}
			defer encrypted.Close()
			found, err := encrypted.Search(context.Background(), msg.ChatID, "archivelegacycanary", 10)
			if err != nil || len(found) != 1 || found[0].Text != msg.Text || found[0].UserID != msg.UserID || found[0].Username != msg.Username || found[0].DisplayName != msg.DisplayName || !found[0].Date.Equal(msg.Date) {
				t.Fatalf("migration lost message: %+v, %v", found, err)
			}
			if legacyFTS && found[0].ID != 42 {
				t.Fatal("legacy row ID changed")
			}
			var residual int
			if err := encrypted.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'messages_fts%' OR name = 'messages_migration'`).Scan(&residual); err != nil || residual != 0 {
				t.Fatalf("plaintext tables remain: %d, %v", residual, err)
			}
			if err := encrypted.db.QueryRow(`SELECT count(*) FROM chatlog_metadata WHERE name = 'cleanup_pending'`).Scan(&residual); err != nil || residual != 0 {
				t.Fatalf("cleanup unfinished: %d, %v", residual, err)
			}
			assertArchivePrivate(t, path, "archivelegacycanary", "erasedwordcanary", msg.Username, msg.DisplayName)
			if err := encrypted.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, 100, key)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if recent, err := reopened.Recent(context.Background(), msg.ChatID, 50); err != nil || len(recent) != 1 || recent[0].Text != msg.Text {
				t.Fatalf("migration restart: %+v, %v", recent, err)
			}
		})
	}
}

func assertArchivePrivate(t *testing.T, path string, secrets ...string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		stat, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Mode().Perm() != 0o600 {
			t.Fatalf("archive file permissions: %s %o", suffix, stat.Mode().Perm())
		}
		for _, secret := range secrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("plaintext remained in archive%s", suffix)
			}
		}
	}
}

func TestChatArchiveMigrationRollbackAndCleanupResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	store, err := Open(path, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := Message{ChatID: 1, MessageID: 1, Text: "rollbackcanary", Date: time.Unix(1700000000, 0).UTC()}
	if err := store.Save(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO messages(chat_id,message_id,date,payload) VALUES (1,2,1700000001,'invalid JSON')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{4}, 32)
	if failed, err := Open(path, 100, key); err == nil {
		_ = failed.Close()
		t.Fatal("migration accepted a damaged record")
	}
	store, err = Open(path, 100, nil)
	if err != nil {
		t.Fatalf("failed migration changed the original format: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failed migration lost rows: %d, %v", count, err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'messages_migration'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed migration left a replacement table")
	}
	if _, err := store.db.Exec(`DELETE FROM messages WHERE message_id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, 100, key)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process interruption after the migration committed but before cleanup finished.
	if _, err := store.db.Exec(`INSERT INTO chatlog_metadata(name,value) VALUES ('cleanup_pending','1')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, 100, key)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.db.QueryRow(`SELECT count(*) FROM chatlog_metadata WHERE name = 'cleanup_pending'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("interrupted cleanup was not resumed")
	}
	recent, err := store.Recent(context.Background(), 1, 10)
	if err != nil || len(recent) != 1 || recent[0].Text != msg.Text {
		t.Fatalf("cleanup lost message: %+v, %v", recent, err)
	}
	assertArchivePrivate(t, path, msg.Text, "invalid JSON")
}
