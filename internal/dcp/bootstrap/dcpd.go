package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/usvc-dev/stdtypes/pkg/process"
)

// Starts the DCPd (API server) process.
// Returns the ProcessExitInfo channel that tells the fate of the API server process,
// the API server process ID (if startup is successful), and an error, if any.
func RunDcpD(ctx context.Context, dcpdPath string) (<-chan process.ProcessExitInfo, int32, error) {
	const dcpdExeNotFound = "Could not determine the path to dcpd executable: %w"

	if dcpdPath == "" {
		ex, err := os.Executable()
		if err != nil {
			return nil, process.UnknownPID, fmt.Errorf(dcpdExeNotFound, err)
		}
		dir := filepath.Dir(ex)

		if isWindows() {
			dcpdPath = filepath.Join(dir, "dcpd.exe")
		} else {
			dcpdPath = filepath.Join(dir, "dcpd")
		}
	}

	info, err := os.Stat(dcpdPath)
	if err != nil {
		return nil, process.UnknownPID, fmt.Errorf(dcpdExeNotFound, err)
	}
	if info.IsDir() {
		return nil, process.UnknownPID, fmt.Errorf("Path '%s' points to a directory (expected DCPd executable)", dcpdPath)
	}

	pc := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pc)
	cmd := exec.CommandContext(ctx, dcpdPath)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Args = []string{
		dcpdPath,
	}

	executor := process.NewOSExecutor()
	pid, startWaitForProcessExit, err := executor.StartProcess(ctx, cmd, peh)
	if err != nil {
		return nil, process.UnknownPID, fmt.Errorf("could not launch DCPd process: %w", err)
	}

	startWaitForProcessExit()
	return pc, pid, nil
}

func isWindows() bool {
	return runtime.GOOS == "windows"
}
