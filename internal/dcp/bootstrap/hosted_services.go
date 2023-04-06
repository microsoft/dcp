package bootstrap

import (
	"os"
	"os/exec"

	"github.com/usvc-dev/apiserver/internal/hosting"
	"github.com/usvc-dev/stdtypes/pkg/process"
)

func NewDcpExtensionService(kubeconfigPath string, appRootDir string, ext DcpExtension) (*hosting.CommandService, error) {
	cmd := exec.Command(ext.Path, "--kubeconfig", kubeconfigPath)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Dir = appRootDir
	return hosting.NewCommandService(ext.Name, cmd, process.NewOSExecutor()), nil
}
