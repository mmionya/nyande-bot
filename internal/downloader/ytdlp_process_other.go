//go:build !unix

package downloader

import "os/exec"

func configureYTDLPProcess(command *exec.Cmd) {}
