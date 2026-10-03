package access

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ponytail: bans gate new work; add per-user cancellation if in-flight work must stop too.
type Store struct {
	mu     sync.RWMutex
	path   string
	owners map[string]bool
	banned map[string]bool
}

// ParseUserID accepts only canonical positive decimal uint64 IDs.
func ParseUserID(raw string) (string, error) {
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != raw {
		return "", errors.New("user ID must be a positive decimal number without signs or leading zeros")
	}
	return raw, nil
}

func Open(path string, owners []string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("banned users file path is empty")
	}
	s := &Store{path: path, owners: make(map[string]bool), banned: make(map[string]bool)}
	for _, raw := range owners {
		id, err := ParseUserID(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid bot owner ID %q: %w", raw, err)
		}
		s.owners[id] = true
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("decode banned users: %w", err)
	}
	if ids == nil {
		return nil, errors.New("banned users file must contain a JSON array of user IDs")
	}
	for _, raw := range ids {
		id, err := ParseUserID(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid banned user ID %q: %w", raw, err)
		}
		if !s.IsOwner(id) {
			s.banned[id] = true
		}
	}
	return s, nil
}

func (s *Store) IsOwner(id string) bool { return s != nil && s.owners[id] }

func (s *Store) IsBanned(id string) bool {
	if s == nil || s.IsOwner(id) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.banned[id]
}

func (s *Store) SetBanned(id string, banned bool) error {
	if _, err := ParseUserID(id); err != nil {
		return err
	}
	if s == nil {
		return errors.New("banned users store is unavailable")
	}
	if banned && s.IsOwner(id) {
		return errors.New("cannot ban a bot owner")
	}
	// ponytail: serialize whole-file writes; move to SQLite if ban lists or writes become large.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.banned[id] == banned {
		return nil
	}
	ids := make([]string, 0, len(s.banned)+1)
	for existing := range s.banned {
		if existing != id {
			ids = append(ids, existing)
		}
	}
	if banned {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	data, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := os.Rename(temporary, s.path); err != nil {
		return err
	}
	if banned {
		s.banned[id] = true
	} else {
		delete(s.banned, id)
	}
	return nil
}
