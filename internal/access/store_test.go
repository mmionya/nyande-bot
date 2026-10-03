package access

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestParseUserID(t *testing.T) {
	for _, id := range []string{"1", "123456789", "18446744073709551615"} {
		if got, err := ParseUserID(id); err != nil || got != id {
			t.Errorf("ParseUserID(%q) = %q, %v", id, got, err)
		}
	}
	for _, id := range []string{"", "0", "01", "+1", "-1", "1.0", "1e2", " 1", "1 ", "１", "18446744073709551616"} {
		if _, err := ParseUserID(id); err == nil {
			t.Errorf("accepted invalid ID %q", id)
		}
	}
}

func TestStorePersistsBansAndProtectsOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "bans.json")
	s, err := Open(path, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsOwner("1") || s.IsOwner("2") || s.IsBanned("2") {
		t.Fatal("unexpected initial access")
	}
	if err := s.SetBanned("1", true); err == nil || s.IsBanned("1") {
		t.Fatal("owner ban succeeded")
	}
	if err := s.SetBanned("0", true); err == nil {
		t.Fatal("invalid ID accepted")
	}
	for _, id := range []string{"2", "3", "2"} {
		if err := s.SetBanned(id, true); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(path, []string{"1", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsBanned("2") || s.IsBanned("3") {
		t.Fatal("ban did not persist or newly configured owner stayed banned")
	}
	for range 2 {
		if err := s.SetBanned("2", false); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(path, nil)
	if err != nil || s.IsBanned("2") || s.IsBanned("3") {
		t.Fatalf("unban did not persist: %v", err)
	}
	var absent *Store
	if absent.IsOwner("1") || absent.IsBanned("1") || absent.SetBanned("1", true) == nil {
		t.Fatal("unexpected nil-store behavior")
	}
}

func TestOpenRejectsInvalidFilesAndOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	for _, data := range []string{"", "null", "{}", "[", `[1]`, `["0"]`, `["01"]`, `["-1"]`, `["18446744073709551616"]`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, nil); err == nil {
			t.Errorf("accepted invalid file %q", data)
		}
	}
	if _, err := Open(t.TempDir(), nil); err == nil {
		t.Fatal("ignored file read failure")
	}
	if _, err := Open("", nil); err == nil {
		t.Fatal("accepted empty path")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.json"), []string{"invalid"}); err == nil {
		t.Fatal("accepted invalid owner")
	}
}

func TestFailedSaveKeepsAccessUnchanged(t *testing.T) {
	for _, stage := range []string{"write", "rename"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bans.json")
			s, err := Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetBanned("2", true); err != nil {
				t.Fatal(err)
			}
			saved := path
			obstacle := path + ".tmp"
			if stage == "rename" {
				saved = path + ".saved"
				if err := os.Rename(path, saved); err != nil {
					t.Fatal(err)
				}
				obstacle = path
			}
			if err := os.Mkdir(obstacle, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := s.SetBanned("2", false); err == nil || !s.IsBanned("2") {
				t.Fatal("failed unban changed access")
			}
			if err := s.SetBanned("3", true); err == nil || s.IsBanned("3") {
				t.Fatal("failed ban changed access")
			}
			persisted, err := Open(saved, nil)
			if err != nil || !persisted.IsBanned("2") || persisted.IsBanned("3") {
				t.Fatalf("failed save changed persisted access: %v", err)
			}
		})
	}
}

func TestConcurrentBans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bans.json")
	s, err := Open(path, []string{"100"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			id := strconv.Itoa(i + 1)
			if err := s.SetBanned(id, true); err != nil {
				t.Error(err)
			}
			if !s.IsBanned(id) || !s.IsOwner("100") {
				t.Error("concurrent access lost")
			}
		})
	}
	wg.Wait()
	persisted, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if !persisted.IsBanned(strconv.Itoa(i + 1)) {
			t.Fatalf("lost concurrent ban for %d", i+1)
		}
	}
}
