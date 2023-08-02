package bootstrap

import (
	"os"
	"os/exec"

	"github.com/microsoft/usvc-apiserver/internal/hosting"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

func NewDcpExtensionService(appRootDir string, ext DcpExtension, command string, invocationFlags []string) (*hosting.CommandService, error) {
	var allArgs []string
	if command != "" {
		allArgs = append(allArgs, command)
	}
	allArgs = append(allArgs, invocationFlags...)
	cmd := exec.Command(ext.Path, allArgs...)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Dir = appRootDir

	// Do not share the process group with dcp CLI process.
	// This allows us, upon reception of Ctrl-C, to delay the shutdown
	// of DCP API server and controllers processes, and perform application shutdown/cleanup
	// before terminating the API server.
	process.DecoupleFromParent(cmd)

	return hosting.NewCommandService(ext.Name, cmd, process.NewOSExecutor(), hosting.CommandServiceRunOptionShowStderr), nil
}
