//go:build windows

package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	// https://learn.microsoft.com/en-us/windows/win32/procthread/process-security-and-access-rights
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
)

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	// https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-queryfullprocessimagenamew
	procQueryFullProcessImageNameW = modkernel32.NewProc("QueryFullProcessImageNameW")

	testEnvProcesses = []string{"kube-apiserver.exe", "etcd.exe"}
)

// On Windows the default test environment implementation sends SIGTERM to the API server and etcd processes.
// This is not supported on Windows, so we need to use a custom implementation.
// Our tests do not use CRDs nor webhooks, so we can skip the cleanup related to these,
// and just shut down the API server and etcd processes.
func stopTestEnvironment() error {
	defer func() {
		// Best effort cleanup of temporary test directories.
		// We need to do this ourselves because we are killing the test environment processes directly,
		// so the normal cleanup logic won't run.
		_ = os.RemoveAll(apiServerCertDir)
		_ = os.RemoveAll(etcdDataDir)
	}()

	chlidren, err := process.GetProcessTree(int32(os.Getpid()))
	if err != nil {
		return fmt.Errorf("failed to get children of the current process: %w", err)
	}

	// Skip the first child, which is the current process.
	for _, childPID := range chlidren[1:] {
		exeName, err := getProcessExeName(uint32(childPID))
		if err != nil {
			return fmt.Errorf("failed to get executable name for process %d: %w", childPID, err)
		}

		if slices.Contains(testEnvProcesses, exeName) {
			process, err := os.FindProcess(int(childPID))
			if err != nil {
				return fmt.Errorf("failed to look up process %s (PID %d): %w", exeName, childPID, err)
			}
			err = process.Kill()
			if err != nil {
				return fmt.Errorf("failed to kill process %s (PID %d): %w", exeName, childPID, err)
			}
		}
	}

	return nil
}

func getProcessExeName(pid uint32) (string, error) {
	handle, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("failed to open process %d: %w", pid, err)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	var exePathBuf [32768]uint16 // 32 kB is the maximum length of a Windows path
	exePathBufLen := uint32(len(exePathBuf))
	err = QueryFullProcessImageName(handle, 0, &exePathBuf[0], &exePathBufLen)
	if err != nil {
		return "", fmt.Errorf("failed to obtain process %d image name: %w", pid, err)
	}

	exePath := syscall.UTF16ToString(exePathBuf[:exePathBufLen])
	exeName := filepath.Base(exePath)
	return exeName, nil
}

func QueryFullProcessImageName(proc syscall.Handle, flags uint32, exeName *uint16, size *uint32) error {
	r1, _, err := syscall.SyscallN(procQueryFullProcessImageNameW.Addr(), uintptr(proc), uintptr(flags), uintptr(unsafe.Pointer(exeName)), uintptr(unsafe.Pointer(size)))
	if r1 == 0 {
		return err
	}
	return nil
}
