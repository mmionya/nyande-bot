package downloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fakeYTDLPProcess(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake extractor uses a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell unavailable")
	}
	path := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunYTDLPSuccessAndDiagnosticTail(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		script := fakeYTDLPProcess(t, `printf '%s\n' "$PYTHONUNBUFFERED"
printf 'downloaded' > "$1/video.mp4"
printf 'finished\n' >&2
`)
		directory := t.TempDir()
		output, err := runYTDLP(context.Background(), script, []string{directory}, directory, 1024)
		if err != nil || string(output) != "1\nfinished\n" {
			t.Fatalf("output=%q err=%v", output, err)
		}
	})
	t.Run("bounded failure output", func(t *testing.T) {
		script := fakeYTDLPProcess(t, `dd if=/dev/zero bs=1024 count=32 2>/dev/null
printf 'ERROR: Requested format is not available\n' >&2
exit 1
`)
		output, err := runYTDLP(context.Background(), script, nil, t.TempDir(), 1024)
		if err == nil || len(output) != ytdlpDiagnosticLimit || !strings.HasSuffix(string(output), "ERROR: Requested format is not available\n") {
			t.Fatalf("diagnostic bytes=%d err=%v", len(output), err)
		}
	})
	// A small, fast download can exit between polls; the final scan must still
	// reject it, including when no yt-dlp size diagnostic was printed.
	t.Run("oversized fast exit", func(t *testing.T) {
		script := fakeYTDLPProcess(t, `dd if=/dev/zero of="$1/video.mp4" bs=2048 count=1 2>/dev/null`)
		directory := t.TempDir()
		_, err := runYTDLP(context.Background(), script, []string{directory}, directory, 1024)
		if !errors.Is(err, errYTDLPMediaTooLarge) {
			t.Fatalf("oversized completed download accepted: %v", err)
		}
	})
}

func TestRunYTDLPStopsUnknownSizeFragmentsAndChildren(t *testing.T) {
	script := fakeYTDLPProcess(t, `sleep 30 &
printf '%s' "$!" > "$2"
printf 'downloading unknown-size fragments\n'
mkdir "$1/fragments"
dd if=/dev/zero of="$1/fragments/video.part" bs=700 count=1 2>/dev/null
dd if=/dev/zero of="$1/fragments/audio.part" bs=700 count=1 2>/dev/null
wait
`)
	directory := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := runYTDLP(ctx, script, []string{directory, pidfile}, directory, 1024)
	if !errors.Is(err, errYTDLPMediaTooLarge) || !strings.Contains(string(output), "unknown-size fragments") {
		t.Fatalf("disk cap did not stop running extractor: output=%q err=%v", output, err)
	}
	assertYTDLPChildStopped(t, pidfile)
}

func TestRunYTDLPCancellation(t *testing.T) {
	script := fakeYTDLPProcess(t, `sleep 30 &
printf '%s' "$!" > "$1"
wait
`)
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runYTDLP(ctx, script, []string{pidfile}, directory, 1024)
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(pidfile); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("extractor child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not preserved: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled extractor did not return promptly")
	}
	assertYTDLPChildStopped(t, pidfile)
}

func assertYTDLPChildStopped(t *testing.T, pidfile string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	pid, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/proc/%s/stat", strings.TrimSpace(string(pid)))
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) || strings.Contains(string(data), ") Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("extractor child %s survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
