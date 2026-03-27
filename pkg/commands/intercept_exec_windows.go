//go:build windows

package commands

import (
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {}

func sendSignalToProcess(cmd *exec.Cmd, signal string) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(os.Kill)
	}
}
