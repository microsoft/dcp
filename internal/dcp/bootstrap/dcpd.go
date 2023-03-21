package bootstrap

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	clientcmd_api "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/usvc-dev/apiserver/internal/kubeconfig"
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

	kubeConfigPath, err := kubeconfig.EnsureKubeconfigFile()
	if err != nil {
		return nil, process.UnknownPID, err
	}

	pc := make(chan process.ProcessExitInfo, 1)
	peh := process.NewChannelProcessExitHandler(pc)
	cmd := exec.CommandContext(ctx, dcpdPath)
	cmd.Env = os.Environ() // Use DCP CLI environment
	cmd.Args = []string{
		dcpdPath,
		"--kubeconfig",
		kubeConfigPath,
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

// TODO: move the function below to kubeconfig package and use from DCPd executable

// Reads Kubeconfig data and returns the port and security token of the current context,
// to be used to configure/connect to API server
func getKubeConfigData(path string) (int, string, error) {
	const InvalidPort = 0
	content, err := os.ReadFile(path)
	if err != nil {
		return InvalidPort, "", err
	}

	var kubeConfig clientcmd_api.Config
	if err = yaml.Unmarshal(content, &kubeConfig); err != nil {
		return InvalidPort, "", err
	}

	kubeContext, found := kubeConfig.Contexts[kubeConfig.CurrentContext]
	if !found {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the context named '%s' (current context) does not exist", kubeConfig.CurrentContext)
	}

	user, found := kubeConfig.AuthInfos[kubeContext.AuthInfo]
	if !found {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the user named '%s' (referred by current context) does not exist", kubeContext.AuthInfo)
	}
	if user.Token == "" {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the user named '%s' (referred by current context) is missing security token information ('token' property)", kubeContext.AuthInfo)
	}

	cluster, found := kubeConfig.Clusters[kubeContext.Cluster]
	if !found {
		return InvalidPort, "", fmt.Errorf("Kubeconfig file is invalid; the user named '%s' (referred by current context) does not exist", kubeContext.AuthInfo)
	}

	clusterUrl, err := url.Parse(cluster.Server)
	if err != nil {
		return InvalidPort, "", fmt.Errorf("could not determine the port to use for the API server; the server URL in Kubeconfig file ('%s') is invalid: %w", cluster.Server, err)
	}

	// If the port is missing, Atoi() will return ErrSyntax
	port, err := strconv.Atoi(clusterUrl.Port())
	if err != nil || port <= InvalidPort {
		return InvalidPort, "", fmt.Errorf("could not determine the port to use for the API server; the port information in server URL ('%s') is either missing or invalid: %w", clusterUrl.Port(), err)
	}

	return port, user.Token, nil
}
