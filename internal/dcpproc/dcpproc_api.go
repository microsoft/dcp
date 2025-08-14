package dcpproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

const (
	DCP_SKIP_MONITOR_PROCESSES = "DCP_SKIP_MONITOR_PROCESSES"
)

// Starts the process monitor for the given child process.
func RunWatcher(
	pe process.Executor,
	childPid process.Pid_t,
	childStartTime time.Time,
	log logr.Logger,
) {
	if _, found := os.LookupEnv(DCP_SKIP_MONITOR_PROCESSES); found {
		return
	}

	// This is a best effort and will only log errors if the process watcher can't be started
	binPath, binPathErr := dcppaths.GetDcpBinDir()
	if binPathErr != nil {
		log.Error(binPathErr, "could not resolve path to process monitor", "PID", childPid)
	} else {
		procMonitorPath := filepath.Join(binPath, "dcpproc")
		if runtime.GOOS == "windows" {
			procMonitorPath += ".exe"
		}

		// DCP doesn't shut down if DCPCTRL goes away, but DCPCTRL will shut down if DCP goes away. For now, watching DCPCTRL is the safer bet.

		monitorPid := os.Getpid()
		monitorCmdArgs := []string{
			"--monitor", strconv.Itoa(monitorPid),
			"--child", strconv.FormatInt(int64(childPid), 10),
		}

		pid, pidErr := process.Uint32_ToPidT(uint32(monitorPid))
		if pidErr != nil {
			log.Error(pidErr, "could not validate monitor PID", "PID", monitorPid)
			return
		}

		monitorTime := process.StartTimeForProcess(pid)
		if !monitorTime.IsZero() {
			monitorCmdArgs = append(monitorCmdArgs, "--monitor-start-time", monitorTime.Format(osutil.RFC3339MiliTimestampFormat))
		}

		if !childStartTime.IsZero() {
			monitorCmdArgs = append(monitorCmdArgs, "--child-start-time", childStartTime.Format(osutil.RFC3339MiliTimestampFormat))
		}

		monitorCmd := exec.Command(procMonitorPath, monitorCmdArgs...)
		monitorCmd.Env = os.Environ()    // Use DCP CLI environment
		logger.WithSessionId(monitorCmd) // Ensure the session ID is passed to the monitor command
		_, _, monitorErr := pe.StartAndForget(monitorCmd, process.CreationFlagsNone)
		if monitorErr != nil {
			log.Error(monitorErr, "failed to start process monitor", "executable", procMonitorPath, "PID", childPid)
		}
	}
}
