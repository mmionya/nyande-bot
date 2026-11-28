package bot

import (
	"path/filepath"
	"testing"

	"github.com/mmionya/nyande-bot/internal/privatefile"
)

func privateStorageDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private")
	if err := privatefile.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
