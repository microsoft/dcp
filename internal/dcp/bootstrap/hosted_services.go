package bootstrap

import (
	"os"
	"os/exec"

	"github.com/usvc-dev/apiserver/internal/hosting"
	"github.com/usvc-dev/stdtypes/pkg/process"
)

func NewDcpExtensionService(kubeconfigPath string, appRootDir string, ext DcpExtension, command string) (*hosting.CommandService, error) {
	var allArgs []string
	if command != "" {
		allArgs = append(allArgs, command)
	}
	allArgs = append(allArgs, "--kubeconfig", kubeconfigPath)
	cmd := exec.Command(ext.Path, allArgs...)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Dir = appRootDir
	return hosting.NewCommandService(ext.Name, cmd, process.NewOSExecutor()), nil
}
