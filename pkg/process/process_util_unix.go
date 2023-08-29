//go:build !windows

package process

import (
	"os"
	"os/exec"
	"syscall"
)

// Use separate process group so this process exit will not affect the children.
func DecoupleFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Use separate process group so this process exit will not affect the children.
// This is the same as DecoupleFromParent on Unix systems.
func ForkFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}
}

func FindProcess(pid int32) (*os.Process, error) {
	process, err := os.FindProcess(int(pid))
	if err != nil {
		return nil, err
	}

	// Check if the process actually exists for Unix systems
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return nil, err
	}

	return process, nil
}
