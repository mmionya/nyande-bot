//go:build unix

package downloader

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// yt-dlp can spawn FFmpeg and JavaScript runtimes. Kill its whole group when
// cancellation or the disk limit stops the download.
func configureYTDLPProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
