package integration_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/dcpclient"
	"github.com/microsoft/usvc-apiserver/internal/exerunners"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var (
	testProcessExecutor *ctrl_testutil.TestProcessExecutor
	ideRunner           *ctrl_testutil.TestIdeRunner
	client              ctrl_client.Client
	orchestrator        *ctrl_testutil.TestContainerOrchestrator
)

const (
	pollImmediately = true // Don't wait before polling for the first time
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
	dcpdPath, dcpdPathErr := getDcpdExecutablePath()
	if dcpdPathErr != nil {
		return fmt.Errorf("failed to find the DCPD executable: %w", dcpdPathErr)
	}

	suffix, randErr := randdata.MakeRandomString(8)
	if randErr != nil {
		return fmt.Errorf("failed to generate random string for kubeconfig file suffix: %w", randErr)
	}
	kubeconfigPath := filepath.Join(testutil.TestTempRoot(), fmt.Sprintf("kubeconfig-test-%s", suffix))
	if kubeconfigErr := kubeconfig.EnsureKubeconfigFile(kubeconfigPath); kubeconfigErr != nil {
		return kubeconfigErr
	}

	// We are going to stop the API server only after all the controller manager is done.
	// This avoids a bunch of shutdown errors from the manager.
	dcpdCtx, stopDcpd := context.WithCancel(context.Background())

	// This is initially set to allow quick and clean shutdown if some of the initialization code below fails,
	// but we will reset when the manager starts.
	managerDone := concurrency.NewAutoResetEvent(true)
	_ = context.AfterFunc(ctx, func() {
		<-managerDone.Wait()
		stopDcpd()
	})

	cmd := exec.CommandContext(ctx, dcpdPath, "--kubeconfig", kubeconfigPath)
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

	_, startWaitForProcessExit, dcpdStartErr := pe.StartProcess(dcpdCtx, cmd, apiserverExitHandler)
	if dcpdStartErr != nil {
		_ = os.Remove(kubeconfigPath)
		return fmt.Errorf("failed to start the API server process: %w", dcpdStartErr)
	}
	startWaitForProcessExit()

	config, configErr := dcpclient.NewConfigFromKubeconfigFile(kubeconfigPath)
	if configErr != nil {
		return fmt.Errorf("failed to build client-go config: %w", configErr)
	}

	// Using generous timeout because the client factory is going to interrogate the API server that we just have started.
	var clientErr error
	client, clientErr = dcpclient.NewClientFromKubeconfigFile(ctx, 20*time.Second, config)
	if clientErr != nil {
		return fmt.Errorf("failed to create controller-runtime client: %w", clientErr)
	}

	opts := controllers.NewControllerManagerOptions(ctx, client.Scheme(), log)
	mgr, err := ctrl.NewManager(config, opts)
	if err != nil {
		return fmt.Errorf("failed to initialize controller manager: %w", err)
	}

	testProcessExecutor = ctrl_testutil.NewTestProcessExecutor(ctx)
	orchestrator = ctrl_testutil.NewTestContainerOrchestrator(ctrl.Log.WithName("TestContainerOrchestrator"))
	exeRunner := exerunners.NewProcessExecutableRunner(testProcessExecutor)
	ideRunner = ctrl_testutil.NewTestIdeRunner(ctx)

	execR := controllers.NewExecutableReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ExecutableReconciler"),
		map[apiv1.ExecutionType]controllers.ExecutableRunner{
			apiv1.ExecutionTypeProcess: exeRunner,
			apiv1.ExecutionTypeIDE:     ideRunner,
		},
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
		orchestrator,
	)
	if err = networkR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Network reconciler: %w", err)
	}

	containerR := controllers.NewContainerReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ContainerReconciler"),
		orchestrator,
	)
	if err = containerR.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("failed to initialize Container reconciler: %w", err)
	}

	volumeR := controllers.NewVolumeReconciler(
		mgr.GetClient(),
		ctrl.Log.WithName("VolumeReconciler"),
		orchestrator,
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

func getDcpdExecutablePath() (string, error) {
	dcpdExeName := "dcpd"
	if runtime.GOOS == "windows" {
		dcpdExeName += ".exe"
	}

	outputBin, found := os.LookupEnv("OUTPUT_BIN")
	if found {
		dcpPath := filepath.Join(outputBin, dcppaths.DcpExtensionsDir, dcpdExeName)
		file, err := os.Stat(dcpPath)
		if err != nil {
			return "", fmt.Errorf("failed to find the DCPD executable: %w", err)
		}
		if file.IsDir() {
			return "", fmt.Errorf("the expected path to DCPD executable is a directory: %s", dcpPath)
		}
		return dcpPath, nil
	}

	tail := []string{dcppaths.DcpBinDir, dcppaths.DcpExtensionsDir, dcpdExeName}
	rootFolder, err := testutil.FindRootFor(testutil.FileTarget, tail...)
	if err != nil {
		return "", err
	}

	return filepath.Join(append([]string{rootFolder}, tail...)...), nil
}

func waitObjectAssumesState[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](t *testing.T, ctx context.Context, name types.NamespacedName, isInState func(*T) (bool, error)) *T {
	var updatedObject *T = new(T)

	hasExpectedState := func(ctx context.Context) (bool, error) {
		err := client.Get(ctx, name, PT(updatedObject))
		if ctrl_client.IgnoreNotFound(err) != nil {
			t.Fatalf("unable to fetch the object '%s' from API server: %v", name.Name, err)
			return false, err
		} else if err != nil {
			return false, nil
		}

		return isInState(updatedObject)
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, hasExpectedState)
	if err != nil {
		t.Fatalf("Waiting for object '%s' to assume desired state failed: %v", name.Name, err)
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

func waitServiceReady(t *testing.T, ctx context.Context, svc *apiv1.Service) {
	_ = waitObjectAssumesState(t, ctx, svc.NamespacedName(), func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateReady, nil
	})
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

	return resiliency.Retry(ctx, try)
}
