//go:build linux

package discordbot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMusicCancellationKillsExtractorChildren(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "music-test", pidfile)
	configureMusicProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Always reap the parent, even when the child setup fails.
	defer func() { cancel(); _ = cmd.Wait() }()
	var pid string
	waitMusic(t, func() bool {
		data, err := os.ReadFile(pidfile)
		pid = strings.TrimSpace(string(data))
		return err == nil && pid != ""
	})
	cancel()
	waitMusic(t, func() bool {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%s/stat", pid))
		return os.IsNotExist(err) || strings.Contains(string(data), ") Z")
	})
}
