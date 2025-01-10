package ctrlutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgorest "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/dcpclient"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

// ApiServerInfo contains data for interacting with DCP API server instance.
type ApiServerInfo struct {
	// CONTRACT MEMBERS (can be used by tests and other functions)

	// Stops the API server process and cleans up associated resources.
	Dispose func()

	// The event that will be set when the API server process exits.
	ApiServerExited *concurrency.AutoResetEvent

	// The event that will be set when the cleanup of the API server and associated resources is complete.
	ApiServerDisposalComplete *concurrency.AutoResetEvent

	// Strongly-typed client for the API server.
	Client ctrl_client.Client

	// REST client for the API server.
	RestClient *clientgorest.RESTClient

	// Configuration for the client used to connect to the API server.
	ClientConfig *clientgorest.Config

	// Container orchestrator used by the API server.
	ContainerOrchestrator *TestContainerOrchestrator

	// NON-CONTRACT MEMBERS (used internally by the startApiServer function)

	// Path to the kubeconfig file used to connect to the API server.
	kubeconfigPath string

	// The execution context and the execution context cancel function for the API server process.
	dcpCtx context.Context

	// Lock allowing safe access from multiple goroutines.
	lock *sync.Mutex
}

// Starts the API server in a separate process.
func StartApiServer(testRunCtx context.Context, log logr.Logger) (*ApiServerInfo, error) {
	info := ApiServerInfo{
		lock:                      &sync.Mutex{},
		ApiServerExited:           concurrency.NewAutoResetEvent(false),
		ApiServerDisposalComplete: concurrency.NewAutoResetEvent(false),
	}

	cleanup := func() {
		info.lock.Lock()
		defer info.lock.Unlock()
		defer info.ApiServerDisposalComplete.SetAndFreeze()

		if info.ContainerOrchestrator != nil {
			co := info.ContainerOrchestrator
			info.ContainerOrchestrator = nil
			if coCloseErr := co.Close(); coCloseErr != nil {
				log.Error(coCloseErr, "Failed to close the test container orchestrator")
			}
		}

		if info.RestClient != nil {
			rc := info.RestClient
			info.RestClient = nil

			if !info.ApiServerExited.Frozen() {
				// Do not use testRunCtx because it might be cancelled already at this point.
				// Use a short, fixed timeout instead.
				shutdownCallCtx, shutdownCallCtxCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutdownCallCtxCancel()
				shutdownRequestErr := apiserver.RequestApiServerShutdown(shutdownCallCtx, rc)
				if shutdownRequestErr != nil {
					log.Error(shutdownRequestErr, "Failed to request API server shutdown")
				} else {
					<-info.ApiServerExited.Wait()
				}
			}
		}

		if info.kubeconfigPath != "" {
			path := info.kubeconfigPath
			info.kubeconfigPath = ""
			// Best effort. May be deleted at this point anyway by the API server process cleaning up the session folder.
			_ = os.Remove(path)
		}
	}

	dcpPath, dcpPathErr := getDcpExecutablePath()
	if dcpPathErr != nil {
		return nil, fmt.Errorf("failed to find the DCP executable: %w", dcpPathErr)
	}

	suffix, randErr := randdata.MakeRandomString(8)
	if randErr != nil {
		return nil, fmt.Errorf("failed to generate random string for kubeconfig file suffix: %w", randErr)
	}
	info.kubeconfigPath = filepath.Join(testutil.TestTempDir(), fmt.Sprintf("kubeconfig-test-%s", suffix))

	var dcpCtxCancel context.CancelFunc
	info.dcpCtx, dcpCtxCancel = context.WithCancel(testRunCtx)
	info.Dispose = func() {
		cleanup()
		dcpCtxCancel()
	}

	// From here on we need to do cleanup if something goes wrong.

	tco, tcoErr := NewTestContainerOrchestrator(
		info.dcpCtx,
		ctrl.Log.WithName("TestContainerOrchestrator"),
		TcoOptionEnableSocketListener,
	)
	if tcoErr != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create test container orchestrator: %w", tcoErr)
	}
	info.ContainerOrchestrator = tco

	// Do not use exec.CommandContext() because on Unix-like OSes it will kill the process DEAD
	// immediately after the context is cancelled, preventing dcp from cleaning up properly.
	cmd := exec.Command(dcpPath,
		"start-apiserver",
		"--server-only",
		"--kubeconfig", info.kubeconfigPath,
		"--test-container-log-source", tco.GetSocketFilePath(),
		"--monitor", strconv.Itoa(os.Getpid()),
	)
	env := addToEnvIfPresent(os.Environ(),
		logger.DCP_DIAGNOSTICS_LOG_FOLDER,
		logger.DCP_DIAGNOSTICS_LOG_LEVEL,
		logger.DCP_LOG_SOCKET,
		usvc_io.DCP_SESSION_FOLDER,
		usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS,
		kubeconfig.DCP_SECURE_TOKEN,
	)
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	apiserverExitHandler := process.ProcessExitHandlerFunc(func(_ process.Pid_t, exitCode int32, err error) {
		if errors.Is(err, context.Canceled) {
			// Expected, this is how we cancel the API server process.
		} else if err != nil {
			log.Error(err, "API server process could not be tracked")
		} else if exitCode != 0 && exitCode != process.UnknownExitCode {
			log.Error(fmt.Errorf("API server process exited with non-zero exit code: %d", exitCode), "",
				"Stdout", stdout.String(),
				"Stderr", stderr.String())
		}
		info.ApiServerExited.SetAndFreeze()
	})

	pe := process.NewOSExecutor(log)
	_, _, startWaitForProcessExit, dcpStartErr := pe.StartProcess(testRunCtx, cmd, apiserverExitHandler)
	if dcpStartErr != nil {
		info.ApiServerExited.SetAndFreeze()
		cleanup()
		return nil, fmt.Errorf("failed to start the API server process: %w", dcpStartErr)
	}
	startWaitForProcessExit()

	// Using generous timeout because AzDO pipeline machines can be very slow at times.
	const configCreationTimeout = 40 * time.Second
	const clientCreationTimeout = 20 * time.Second
	configCreationCtx, configCreationCancel := context.WithTimeout(info.dcpCtx, configCreationTimeout)
	defer configCreationCancel()

	clientConfig, clientConfigErr := dcpclient.NewConfigFromKubeconfigFile(configCreationCtx, info.kubeconfigPath)
	if clientConfigErr != nil {
		info.Dispose()
		return nil, fmt.Errorf("failed to build client-go config: %w", clientConfigErr)
	}

	info.ClientConfig = clientConfig

	clientCreationCtx, clientCreationCancel := context.WithTimeout(info.dcpCtx, clientCreationTimeout)
	defer clientCreationCancel()
	client, clientErr := dcpclient.NewClientFromConfig(clientCreationCtx, info.ClientConfig)
	if clientErr != nil {
		info.Dispose()
		return nil, fmt.Errorf("failed to create controller-runtime client: %w", clientErr)
	}
	info.Client = client

	restClientConfig := clientgorest.CopyConfig(info.ClientConfig)
	restClientConfig.GroupVersion = &apiv1.GroupVersion
	restClientConfig.NegotiatedSerializer = serializer.NewCodecFactory(dcpclient.NewScheme())
	restClientConfig.APIPath = "/apis"
	restClient, restClientErr := clientgorest.RESTClientFor(restClientConfig)
	if restClientErr != nil {
		info.Dispose()
		return nil, fmt.Errorf("failed to create raw (client-go REST) Kubernetes client: %w", restClientErr)
	}
	info.RestClient = restClient

	return &info, nil
}

func getDcpExecutablePath() (string, error) {
	dcpExeName := "dcp"
	if runtime.GOOS == "windows" {
		dcpExeName += ".exe"
	}

	outputBin, found := os.LookupEnv("OUTPUT_BIN")
	if found {
		dcpPath := filepath.Join(outputBin, dcpExeName)
		file, err := os.Stat(dcpPath)
		if err != nil {
			return "", fmt.Errorf("failed to find the DCP executable: %w", err)
		}
		if file.IsDir() {
			return "", fmt.Errorf("the expected path to DCP executable is a directory: %s", dcpPath)
		}
		return dcpPath, nil
	}

	tail := []string{dcppaths.DcpBinDir, dcpExeName}
	rootFolder, err := testutil.FindRootFor(testutil.FileTarget, tail...)
	if err != nil {
		return "", err
	}

	return filepath.Join(append([]string{rootFolder}, tail...)...), nil
}

func addToEnvIfPresent(env []string, vars ...string) []string {
	retval := env

	for _, varName := range vars {
		value, found := os.LookupEnv(varName)
		if found {
			value = strings.TrimSpace(value)
			if value != "" {
				retval = append(retval, fmt.Sprintf("%s=%s", varName, value))
			}
		}
	}

	return retval
}
