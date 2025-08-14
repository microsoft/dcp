package bootstrap

import (
	"os"
	"os/exec"
	"strconv"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/hosting"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

func NewDcpExtensionService(appRootDir string, ext DcpExtension, command string, invocationFlags []string, log logr.Logger) (*hosting.CommandService, error) {
	var allArgs []string
	if command != "" {
		allArgs = append(allArgs, command)
	}
	allArgs = append(allArgs, invocationFlags...)
	isProcessMonitor := slices.Contains(ext.Capabilities, extensions.ProcessMonitorCapability)
	if isProcessMonitor {
		allArgs = append(allArgs, "--monitor", strconv.Itoa(os.Getpid()))
	}
	cmd := exec.Command(ext.Path, allArgs...)
	cmd.Env = os.Environ()    // Use DCP CLI environment
	logger.WithSessionId(cmd) // Ensure the session ID is passed to the command
	cmd.Dir = appRootDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Do not share the process group with dcp CLI process.
	// This allows us, upon reception of Ctrl-C, to delay the shutdown
	// of DCP API server and controllers processes, and perform application shutdown/cleanup
	// before terminating the API server.
	process.DecoupleFromParent(cmd)

	hostingOpts := hosting.CommandServiceRunOptionShowStderr
	if isProcessMonitor {
		hostingOpts |= hosting.CommandServiceRunOptionDontTerminate
	}
	return hosting.NewCommandService(ext.Name, cmd, process.NewOSExecutor(log), hostingOpts, log), nil
}
