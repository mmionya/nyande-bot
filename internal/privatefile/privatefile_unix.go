//go:build !windows

package privatefile

import (
	"os"
	"path/filepath"
)

// Check validates the permissions on the already opened file.
func Check(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return ErrPermissions
	}
	return nil
}

func Protect(path string) error { return os.Chmod(path, 0o600) }

func MkdirAll(path string, mode os.FileMode) error { return os.MkdirAll(path, mode) }

func CreateTemp(directory, pattern string) (*os.File, error) {
	return os.CreateTemp(directory, pattern)
}

// Open creates a private file or restricts an existing file before use.
func Open(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Replace publishes a flushed temporary file and syncs its directory entry.
func Replace(source, target string) error {
	parent, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return parent.Sync()
}
