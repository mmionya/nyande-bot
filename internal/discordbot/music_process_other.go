//go:build !unix

package discordbot

import "os/exec"

func configureMusicProcess(cmd *exec.Cmd) {}
