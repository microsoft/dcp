//go:build windows

package process

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Use separate process group so this process exit will not affect the children.
func DecoupleFromParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Separate the child from the parent to the largest extent possible by: (1) using separate console,
// (2) using separate process group, and (3) asking to be detached from the parent process job,
// so that the child process is not killed when the parent process job is closed
// (assuming JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE). For the job detachment to be effective,
// the job has to allow it via JOB_OBJECT_LIMIT_BREAKAWAY_OK.
func ForkFromParent(cmd *exec.Cmd) {
	creationFlags := uint32(windows.CREATE_NEW_CONSOLE | syscall.CREATE_NEW_PROCESS_GROUP)
	if breakaway, err := canBreakAwayFromJob(); err == nil && breakaway {
		creationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: creationFlags,
		HideWindow:    true,
	}
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

func canBreakAwayFromJob() (bool, error) {
	// Check if the process can break away from the job
	jobObject, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(jobObject)

	var jobInformation windows.JOBOBJECT_BASIC_LIMIT_INFORMATION
	err = windows.QueryInformationJobObject(jobObject, windows.JobObjectBasicLimitInformation, uintptr(unsafe.Pointer(&jobInformation)), uint32(unsafe.Sizeof(jobInformation)), nil)
	if err != nil {
		return false, err
	}

	return jobInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0, nil
}
