package bootstrap

import (
	"os"
	"os/exec"

	"github.com/usvc-dev/apiserver/internal/dcp/extensions"
	"github.com/usvc-dev/apiserver/internal/hosting"
	"github.com/usvc-dev/stdtypes/pkg/process"
)

const ApiServerServiceName = "API Server"

// Creates a CommandService struct that represents the API server (running as a separate process).
func NewDcpdService(kubeconfigPath string, appRootDir string) (*hosting.CommandService, error) {
	dcpdPath, err := extensions.GetDcpdPath()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(dcpdPath, "--kubeconfig", kubeconfigPath)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Dir = appRootDir
	return hosting.NewCommandService(ApiServerServiceName, cmd, process.NewOSExecutor()), nil
}

func NewControllerService(kubeconfigPath string, appRootDir string, controller extensions.DcpExtension) (*hosting.CommandService, error) {
	cmd := exec.Command(controller.Path, "--kubeconfig", kubeconfigPath)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Dir = appRootDir
	return hosting.NewCommandService(controller.Name, cmd, process.NewOSExecutor()), nil
}
