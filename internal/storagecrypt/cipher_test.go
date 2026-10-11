package storagecrypt

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticatedStorageAndKeyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage.key")
	key := bytes.Repeat([]byte{42}, 32)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadKeyFile(path)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("key read failed: %v", err)
	}
	c, err := New(loaded)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, aad := []byte("private message"), []byte("chat:1:message:2")
	sealed := c.Seal(plaintext, aad)
	if bytes.Contains(sealed, plaintext) || bytes.Equal(sealed, c.Seal(plaintext, aad)) {
		t.Fatal("ciphertext must hide plaintext and use a fresh nonce")
	}
	opened, err := c.Open(sealed, aad)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if _, err := c.Open(sealed, []byte("chat:2:message:2")); err == nil {
		t.Fatal("accepted swapped ciphertext")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := c.Open(sealed, aad); err == nil {
		t.Fatal("accepted tampered ciphertext")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKeyFile(path); err == nil {
		t.Fatal("accepted world-readable key")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("accepted invalid key length")
	}
}
