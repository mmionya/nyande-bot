//go:build unix

package discordbot

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// yt-dlp can spawn a JavaScript runtime. Cancel the entire process group so
// those children cannot outlive an interrupted search or stream resolution.
func configureMusicProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
