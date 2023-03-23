package bootstrap

import (
	"os"
	"os/exec"

	"github.com/usvc-dev/apiserver/internal/hosting"
	"github.com/usvc-dev/stdtypes/pkg/process"
)

// Creates a CommandService struct that represents the API server (running as a separate process).
func NewDcpdService(kubeconfigPath string) (*hosting.CommandService, error) {
	dcpdPath, err := GetDcpdPath()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(dcpdPath, "--kubeconfig", kubeconfigPath)
	cmd.Env = os.Environ() // Use DCP CLI environment
	return hosting.NewCommandService("API Server", cmd, process.NewOSExecutor()), nil
}
