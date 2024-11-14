//go:build !windows

package process

import (
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
