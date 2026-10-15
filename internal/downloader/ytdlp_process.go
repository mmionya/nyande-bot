package downloader

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const ytdlpDiagnosticLimit = 16 * 1024

// runYTDLP bounds both diagnostics and temporary downloads, including fragments
// whose eventual size yt-dlp cannot check with --max-filesize before downloading.
func runYTDLP(ctx context.Context, executable string, arguments []string, directory string, maxDiskBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxDiskBytes <= 0 {
		return nil, fmt.Errorf("yt-dlp temporary file limit must be positive")
	}
	if err := checkYTDLPDiskUsage(directory, maxDiskBytes); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(runCtx, executable, arguments...)
	command.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	command.WaitDelay = time.Second
	configureYTDLPProcess(command)
	output := &ytdlpDiagnosticTail{data: make([]byte, 0, ytdlpDiagnosticLimit)}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				_ = command.Cancel()
				return output.data, ctx.Err()
			}
			if diskErr := checkYTDLPDiskUsage(directory, maxDiskBytes); diskErr != nil {
				_ = command.Cancel()
				return output.data, diskErr
			}
			if errors.Is(err, exec.ErrWaitDelay) {
				_ = command.Cancel()
			}
			return output.data, err
		case <-ctx.Done():
			cancel()
			_ = command.Cancel()
			<-done
			return output.data, ctx.Err()
		case <-ticker.C:
			if err := checkYTDLPDiskUsage(directory, maxDiskBytes); err != nil {
				cancel()
				_ = command.Cancel()
				<-done
				return output.data, err
			}
		}
	}
}

func checkYTDLPDiskUsage(directory string, maximum int64) error {
	var total int64
	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		// Merging can remove fragment files while the directory is being scanned.
		if os.IsNotExist(err) && path != directory {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Size() > maximum-total {
			return fmt.Errorf("%w: temporary downloads exceed %d bytes", errYTDLPMediaTooLarge, maximum)
		}
		total += info.Size()
		return nil
	})
}

// Stdout and Stderr share this writer, so os/exec serializes all writes.
type ytdlpDiagnosticTail struct {
	data []byte
}

func (tail *ytdlpDiagnosticTail) Write(data []byte) (int, error) {
	length := len(data)
	if length >= ytdlpDiagnosticLimit {
		tail.data = append(tail.data[:0], data[length-ytdlpDiagnosticLimit:]...)
		return length, nil
	}
	if extra := len(tail.data) + length - ytdlpDiagnosticLimit; extra > 0 {
		tail.data = tail.data[:copy(tail.data, tail.data[extra:])]
	}
	tail.data = append(tail.data, data...)
	return length, nil
}
