//go:build windows

package process

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Use separate process group so this process exit will not affect the children.
func DecoupleFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Use separate console group to force the child process completely outside its parent's process group
func ForkFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}

func FindProcess(pid Pid_t) (*os.Process, error) {
	osPid, err := PidT_ToInt(pid)
	if err != nil {
		return nil, err
	}

	process, err := os.FindProcess(osPid)
	if err != nil {
		return nil, err
	}

	return process, nil
}
