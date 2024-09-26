package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	std_slices "slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgorest "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/dcpclient"
	"github.com/microsoft/usvc-apiserver/internal/exerunners"
	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	internal_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var (
	testProcessExecutor   *ctrl_testutil.TestProcessExecutor
	ideRunner             *ctrl_testutil.TestIdeRunner
	client                ctrl_client.Client
	restClient            *clientgorest.RESTClient
	containerOrchestrator *ctrl_testutil.TestContainerOrchestrator
)

const (
	pollImmediately = true // Don't wait before polling for the first time

	// Set to true to enable tests that require access to all network interfaces and try to open ports
	// that are accessible to requests originating from outside of the machine.
	// This is disabled by default because on Windows it causes a security prompt every time the tests are run.
	DCP_TEST_ENABLE_ALL_NETWORK_INTERFACES = "DCP_TEST_ENABLE_ALL_NETWORK_INTERFACES"
	skippingAllNetworkInterfacesTests      = "Skipping test requiring access to all network interfaces and ability to create externally-accessible endpoints"
)

func TestMain(m *testing.M) {
	log := logger.New("test")
	log.SetLevel(zapcore.ErrorLevel)
	if !flag.Parsed() {
		flag.Parse() // Needed to test if verbose flag was present.
	}
	if testing.Verbose() {
		log.SetLevel(zapcore.DebugLevel)
	}
	ctrl.SetLogger(log.Logger)

	networking.EnableStrictMruPortHandling(log.Logger)

	ctx, cancel := context.WithCancel(
		context.WithValue(context.Background(), controllers.ServiceReconcilerProxyHandling, controllers.DoNotStartProxies),
	)
	apiServerExited := concurrency.NewAutoResetEvent(false)
	stopTestEnvironment := func() {
		cancel()

		// Wait for the API server to exit and cleanups to be done, but with a couple seconds timeout.
		select {
		case <-apiServerExited.Wait():
		case <-time.After(2 * time.Second):
		}
	}

	err := startTestEnvironment(ctx, log.Logger, apiServerExited)
	if err != nil {
		stopTestEnvironment()
		panic(err)
	}

	var code int = 0
	defer func() {
		stopTestEnvironment()
		os.Exit(code)
	}()
	code = m.Run()
}

// Starts the DCP API server (separate process) and standard controllers (in-proc).
// Returns the DCP API server process ID or an error.
func startTestEnvironment(ctx context.Context, log logr.Logger, onApiServerExited *concurrency.AutoResetEvent) error {
	dcpPath, dcpPathErr := getDcpExecutablePath()
	if dcpPathErr != nil {
		return fmt.Errorf("failed to find the DCP executable: %w", dcpPathErr)
	}

	suffix, randErr := randdata.MakeRandomString(8)
	if randErr != nil {
		return fmt.Errorf("failed to generate random string for kubeconfig file suffix: %w", randErr)
	}
	kubeconfigPath := filepath.Join(testutil.TestTempRoot(), fmt.Sprintf("kubeconfig-test-%s", suffix))
	if kubeconfigErr := kubeconfig.EnsureKubeconfigFile(kubeconfigPath, log); kubeconfigErr != nil {
		return kubeconfigErr
	}

	var orchestratorErr error
	containerOrchestrator, orchestratorErr = ctrl_testutil.NewTestContainerOrchestrator(ctx, ctrl.Log.WithName("TestContainerOrchestrator"))
	if orchestratorErr != nil {
		return fmt.Errorf("failed to create test container orchestrator: %w", orchestratorErr)
	}

	// We are going to stop the API server only after all the controller manager is done.
	// This avoids a bunch of shutdown errors from the manager.
	dcpCtx, stopDcp := context.WithCancel(context.Background())

	// This is initially set to allow quick and clean shutdown if some of the initialization code below fails,
	// but we will reset when the manager starts.
	managerDone := concurrency.NewAutoResetEvent(true)
	_ = context.AfterFunc(ctx, func() {
		<-managerDone.Wait()
		stopDcp()
		containerOrchestrator.Close()
	})

	const authTokenLength = 32
	bearerToken, err := randdata.MakeRandomString(authTokenLength)
	if err != nil {
		return fmt.Errorf("could not generate authentication token for the DCP API server: %w", err)
	}

	// Do not use exec.CommandContext() because on Unix-like OSes it will kill the process DEAD
	// immediately after the context is cancelled, preventing dcp from cleaning up properly.
	cmd := exec.Command(dcpPath,
		"start-apiserver",
		"--server-only",
		"--kubeconfig", kubeconfigPath,
		"--test-container-log-source", containerOrchestrator.GetSocketFilePath(),
		"--monitor", strconv.Itoa(os.Getpid()),
	)
	cmd.Env = []string{fmt.Sprintf("%s=%s", kubeconfig.DCP_SECURE_TOKEN, string(bearerToken))}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	pe := process.NewOSExecutor(log)
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
		_ = os.Remove(kubeconfigPath) // Best effort
		onApiServerExited.Set()
	})

	_, startWaitForProcessExit, dcpStartErr := pe.StartProcess(dcpCtx, cmd, apiserverExitHandler)
	if dcpStartErr != nil {
		_ = os.Remove(kubeconfigPath)
		return fmt.Errorf("failed to start the API server process: %w", dcpStartErr)
	}
	startWaitForProcessExit()

	clientConfig, clientConfigErr := dcpclient.NewConfigFromKubeconfigFile(kubeconfigPath)
	if clientConfigErr != nil {
		return fmt.Errorf("failed to build client-go config: %w", clientConfigErr)
	}

	// Use a pre-generated bearer token for the API server.
	clientConfig.BearerToken = string(bearerToken)

	// Using generous timeout because AzDO pipeline machines can be very slow at times.
	var clientErr error
	client, clientErr = dcpclient.NewClientFromKubeconfigFile(ctx, 60*time.Second, clientConfig)
	if clientErr != nil {
		return fmt.Errorf("failed to create controller-runtime client: %w", clientErr)
	}

	restClientConfig := clientgorest.CopyConfig(clientConfig)
	restClientConfig.GroupVersion = &apiv1.GroupVersion
	restClientConfig.NegotiatedSerializer = serializer.NewCodecFactory(dcpclient.NewScheme())
	restClientConfig.APIPath = "/apis"
	var restClientErr error
	restClient, restClientErr = clientgorest.RESTClientFor(restClientConfig)
	if restClientErr != nil {
		return fmt.Errorf("failed to create raw (client-go REST) Kubernetes client: %w", restClientErr)
	}

	opts := controllers.NewControllerManagerOptions(ctx, client.Scheme(), log)
	mgr, err := ctrl.NewManager(clientConfig, opts)
	if err != nil {
		return fmt.Errorf("failed to initialize controller manager: %w", err)
	}

	testProcessExecutor = ctrl_testutil.NewTestProcessExecutor(ctx)
	exeRunner := exerunners.NewProcessExecutableRunner(testProcessExecutor)
	ideRunner = ctrl_testutil.NewTestIdeRunner(ctx)

	hpSet := health.NewHealthProbeSet(
		ctx,
		log.WithName("HealthProbeSet"),
		map[apiv1.HealthProbeType]health.HealthProbeExecutor{
			apiv1.HealthProbeTypeHttp: health.HealthProbeExecutorFunc(health.ExecuteHttpProbe),
		},
	)

	execR := controllers.NewExecutableReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ExecutableReconciler"),
		map[apiv1.ExecutionType]controllers.ExecutableRunner{
			apiv1.ExecutionTypeProcess: exeRunner,
			apiv1.ExecutionTypeIDE:     ideRunner,
		},
		hpSet,
	)
	if err = execR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Executable reconciler: %w", err)
	}

	execrsR := controllers.NewExecutableReplicaSetReconciler(
		mgr.GetClient(),
		ctrl.Log.WithName("ExecutableReplicaSetReconciler"),
	)
	if err = execrsR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize ExecutableReplicaSet reconciler: %w", err)
	}

	networkR := controllers.NewNetworkReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("NetworkReconciler"),
		containerOrchestrator,
	)
	if err = networkR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Network reconciler: %w", err)
	}

	containerR := controllers.NewContainerReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ContainerReconciler"),
		containerOrchestrator,
	)
	if err = containerR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Container reconciler: %w", err)
	}

	containerExecR := controllers.NewContainerExecReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ContainerExecReconciler"),
		containerOrchestrator,
	)
	if err = containerExecR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize ContainerExec reconciler: %w", err)
	}

	volumeR := controllers.NewVolumeReconciler(
		mgr.GetClient(),
		ctrl.Log.WithName("VolumeReconciler"),
		containerOrchestrator,
	)
	if err = volumeR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize ContainerVolume reconciler: %w", err)
	}

	serviceR := controllers.NewServiceReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ServiceReconciler"),
		testProcessExecutor,
	)
	if err = serviceR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Service reconciler: %w", err)
	}

	if err = controllers.SetupEndpointIndexWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Endpoint index: %w", err)
	}

	// Starts the controller manager and all the associated controllers
	managerDone.Clear()
	go func() {
		_ = mgr.Start(ctx)
		managerDone.Set()
	}()

	return nil
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

// unexpectedObjectStateError can be used to provide additional context when an object is not in the expected state.
// In is meant to be used from within isInState() function passed to waitObjectAssumesState().
// Unlike any other error, it won't stop the wait when returned from isInState(),
// but if the wait times out and the object is still not in desired state,
// the test will be terminated with the uexpectedObjectStateError serving as the terminating error.
type unexpectedObjectStateError struct {
	errText string
}

func (e *unexpectedObjectStateError) Error() string { return e.errText }

var _ error = (*unexpectedObjectStateError)(nil)

func waitObjectAssumesState[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](t *testing.T, ctx context.Context, name types.NamespacedName, isInState func(*T) (bool, error)) *T {
	var updatedObject *T = new(T)
	var unexpectedStateErr *unexpectedObjectStateError

	hasExpectedState := func(ctx context.Context) (bool, error) {
		err := client.Get(ctx, name, PT(updatedObject))
		if ctrl_client.IgnoreNotFound(err) != nil {
			t.Fatalf("unable to fetch the object '%s' from API server: %v", name.String(), err)
			return false, err
		} else if err != nil {
			return false, nil
		}

		ok, stateCheckErr := isInState(updatedObject)
		if errors.As(stateCheckErr, &unexpectedStateErr) {
			return ok, nil // Unexpected state error does not stop the wait
		} else {
			return ok, stateCheckErr
		}
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, hasExpectedState)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// If the state check function returns errUnexpectedObjectState, use it.
			if unexpectedStateErr != nil {
				t.Fatalf("Waiting for object '%s' to assume desired state failed: %v", name.String(), unexpectedStateErr)
			}
		}
		t.Fatalf("Waiting for object '%s' to assume desired state failed: %v", name.String(), err)
		return nil // make the compiler happy
	} else {
		return updatedObject
	}
}

func waitObjectDeleted[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](t *testing.T, ctx context.Context, name types.NamespacedName) {
	objectNotFound := func(ctx context.Context) (bool, error) {
		var obj T = *new(T)
		err := client.Get(ctx, name, PT(&obj))
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			} else {
				return false, err
			}
		}
		return false, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, objectNotFound)
	if err != nil {
		t.Fatalf("Object '%s' was not deleted as expected: %v", name.Name, err)
	}
}

func waitServiceReady(t *testing.T, ctx context.Context, svc *apiv1.Service) *apiv1.Service {
	updatedSvc := waitObjectAssumesState(t, ctx, svc.NamespacedName(), func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateReady, nil
	})
	return updatedSvc
}

func retryOnConflict[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](ctx context.Context, name types.NamespacedName, action func(context.Context, PT) error) error {

	try := func() error {
		var apiObject PT = new(T)
		err := client.Get(ctx, name, PT(apiObject))
		if err != nil {
			return resiliency.Permanent(fmt.Errorf("unable to fetch the object '%s' from API server: %w", name.String(), err))
		}

		err = action(ctx, apiObject)
		if apierrors.IsConflict(err) {
			return err // Retry
		} else if err != nil {
			return resiliency.Permanent(err)
		}

		return nil
	}

	return resiliency.RetryExponential(ctx, try)
}

func openLogStream[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](
	ctx context.Context,
	obj PT,
	opts apiv1.LogOptions,
	logStreamOpen *concurrency.AutoResetEvent,
) (io.ReadCloser, error) {
	stream, err := restClient.Get().
		NamespaceIfScoped(obj.GetObjectMeta().Namespace, obj.NamespaceScoped()).
		Resource(obj.GetGroupVersionResource().Resource).
		Name(obj.GetObjectMeta().Name).
		SubResource(apiv1.LogSubresourceName).
		VersionedParams(&opts, apiruntime.NewParameterCodec(dcpclient.NewScheme())).
		Stream(ctx)
	if logStreamOpen != nil {
		logStreamOpen.Set() // Set even if error occurs to unblock the test continuation
	}
	return stream, err
}

func waitForObjectLogs[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](
	ctx context.Context,
	obj PT,
	opts apiv1.LogOptions,
	expectedLines [][]byte,
	logStreamOpen *concurrency.AutoResetEvent,
) error {
	logsArrived := func(receivedLines [][]byte) (bool, error) {
		if len(receivedLines) < len(expectedLines) {
			return false, nil // Not enough data yet
		}

		var matchErr error
		allLinesMatch := std_slices.EqualFunc(receivedLines[len(receivedLines)-len(expectedLines):], expectedLines, func(read, pattern []byte) bool {
			var matched bool
			matched, matchErr = regexp.Match(string(pattern), read)
			return matched
		})
		if matchErr != nil {
			return false, fmt.Errorf("Error occurred while matching logs: %w", matchErr)
		}

		return allLinesMatch, nil
	}

	if opts.Follow {
		// In follow mode we open the log stream once and then scan for expected lines
		// until the expectation is satisfied or the context is cancelled.

		logStream, logStreamErr := openLogStream(ctx, obj, opts, logStreamOpen)
		if logStreamErr != nil {
			return fmt.Errorf("could not get log stream %s for %s '%s': %v",
				opts.String(),
				obj.GetObjectKind().GroupVersionKind().Kind,
				obj.GetObjectMeta().Name,
				logStreamErr)
		}

		scanner := bufio.NewScanner(usvc_io.NewContextReader(ctx, logStream, true /* leverageReadCloser */))
		var logLines [][]byte

		for scanner.Scan() {
			line := scanner.Bytes()
			logLines = append(logLines, line)

			arrived, err := logsArrived(logLines)
			if err != nil {
				return err
			} else if arrived {
				return nil // Expected logs arrived = success
			}
		}

		if scanner.Err() == nil && ctx.Err() == nil {
			return fmt.Errorf("Log stream ended before expected logs arrived for %s '%s'. Logs written so far: %s",
				obj.GetObjectKind().GroupVersionKind().Kind,
				obj.GetObjectMeta().Name,
				string(bytes.Join(logLines, osutil.LineSep())))
		} else {
			return fmt.Errorf("An error occurred while reading logs from %s '%s': %w. Logs written so far: %s",
				obj.GetObjectKind().GroupVersionKind().Kind,
				obj.GetObjectMeta().Name,
				errors.Join(scanner.Err(), ctx.Err()),
				string(bytes.Join(logLines, osutil.LineSep())))
		}
	} else {
		// In non-follow mode we will repeatedly query the logs until we get the expected lines.
		// This deals with the issue that the log stream content may not be available immediately after
		// we simulate writing to the log.

		lastLogContents := []byte{}
		hasExpectedLogLines := func(ctx context.Context) (bool, error) {
			logStream, logStreamErr := openLogStream(ctx, obj, opts, logStreamOpen)
			if logStreamErr != nil {
				return false, fmt.Errorf("could not get log stream %s for %s '%s': %w",
					opts.String(),
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetObjectMeta().Name,
					logStreamErr)
			}
			defer logStream.Close()

			logContents, logReadErr := io.ReadAll(logStream)
			if logReadErr != nil {
				return false, fmt.Errorf("could not read the contents of log stream %s for %s '%s': %w",
					opts.String(),
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetObjectMeta().Name,
					logReadErr)
			}

			lastLogContents = logContents
			logLines := bytes.Split(logContents, osutil.LineSep())
			logLines = std_slices.DeleteFunc(logLines, func(s []byte) bool { return len(s) == 0 }) // Remove empty "lines" from Split() result.

			return logsArrived(logLines)
		}

		err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, hasExpectedLogLines)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("Timeout occurred while waiting for expected logs from %s '%s'. Last log contents: %s",
					obj.GetObjectKind().GroupVersionKind().Kind,
					obj.GetObjectMeta().Name,
					string(lastLogContents))
			} else {
				return fmt.Errorf("Expected logs could not be retrieved: %w", err)
			}
		}
		return nil
	}
}

// Starts a test HTTP server that can be used as a target for HTTP health probes.
// Returns the server's address and a function to make the server respond as healthy or unhealthy.
// By default the server responds with 500 Internal Server Error (unhealthy).
func createTestHealthEndpoint(lifetimeCtx context.Context) (string, func(apiv1.HealthProbeOutcome)) {
	enableHealthyResp := &atomic.Bool{}
	enableHealthyResp.Store(true)
	enableUnhealthyResp := &atomic.Bool{}
	enableUnhealthyResp.Store(true)

	setResponse := func(outcome apiv1.HealthProbeOutcome) {
		switch outcome {
		case apiv1.HealthProbeOutcomeSuccess:
			enableUnhealthyResp.Store(false)
			// No need to set enableHealtyResp to true, it's the last in the response list
			// and its Active flag is always true.
		case apiv1.HealthProbeOutcomeFailure:
			enableUnhealthyResp.Store(true)
		default:
			panic(fmt.Sprintf("Unsupported health probe outcome: %s", outcome))
		}
	}

	const urlPath = "/healthz"
	probeUrl := internal_testutil.ServeHttp(lifetimeCtx, []internal_testutil.RouteSpec{
		{
			Pattern: urlPath,
			Responses: []internal_testutil.ResponseSpec{
				{StatusCode: http.StatusServiceUnavailable, Active: enableUnhealthyResp},
				{StatusCode: http.StatusOK, Active: enableHealthyResp},
			},
		},
	})

	return probeUrl + urlPath, setResponse
}
