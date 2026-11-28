package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateReplacement(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested")
	if err := MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "history.json")
	for _, content := range []string{"initial private data", "replacement private data"} {
		file, err := CreateTemp(directory, ".history.tmp-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(file); err != nil {
			t.Fatal("temporary file must be private before writing", err)
		}
		if _, err := file.WriteString(content); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := Replace(file.Name(), path); err != nil {
			t.Fatal(err)
		}
		stored, err := os.ReadFile(path)
		if err != nil || string(stored) != content {
			t.Fatalf("replacement content: %q, %v", stored, err)
		}
		published, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		checkErr := Check(published)
		_ = published.Close()
		if checkErr != nil {
			t.Fatal("published file permissions", checkErr)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("replacement left temporary files: %v, %v", entries, err)
	}
}
