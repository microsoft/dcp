package integration_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/api/errors"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"sigs.k8s.io/controller-runtime/pkg/envtest"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/docker"
	"github.com/microsoft/usvc-apiserver/internal/exerunners"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

var (
	testEnv          *envtest.Environment
	processExecutor  *ctrl_testutil.TestProcessExecutor
	ideRunner        *ctrl_testutil.TestIdeRunner
	client           ctrl_client.Client
	apiServerCertDir string
	etcdDataDir      string
)

const (
	pollImmediately = true // Don't wait before polling for the first time
)

func TestMain(m *testing.M) {
	log := logger.New("test")
	log.SetLevel(zapcore.ErrorLevel)
	ctrl.SetLogger(log.V(1))

	ctx, cancel := context.WithCancel(context.Background())

	flush, err := startTestEnvironment(ctx, log)
	if err != nil {
		cancel()
		panic(err)
	}

	var code int = 0

	defer func() {
		cancel()
		err = stopTestEnvironment()
		flush()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to stop test environment: %v", err)
		}

		os.Exit(code)
	}()

	code = m.Run()
}

func startTestEnvironment(ctx context.Context, log logger.Logger) (func(), error) {
	flushFn := func() {
		// No op by default
	}

	assetDir, err := ctrl_testutil.GetKubeAssetsDir()
	if err != nil {
		return nil, err
	}

	crdRelativePath := []string{"pkg", "generated", "crd"}
	baseDir, err := testutil.FindRootFor(testutil.DirTarget, crdRelativePath...)
	if err != nil {
		return nil, fmt.Errorf("cloud not find the CRD directory: %w", err)
	}
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(append([]string{baseDir}, crdRelativePath...)...),
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assetDir,
	}

	if !flag.Parsed() {
		flag.Parse() // Needed to test if verbose flag was present.
	}

	if testing.Verbose() {
		log.SetLevel(zapcore.DebugLevel)
	}

	scheme := apiruntime.NewScheme()

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1.AddToScheme(scheme))

	cfg, err := testEnv.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize environment: %w", err)
	}
	// Remember the data directories used by Kube API server and etcd, so we can clean them up later (Windows case).
	apiServerCertDir = testEnv.ControlPlane.APIServer.CertDir
	etcdDataDir = testEnv.ControlPlane.Etcd.DataDir

	// Direct-access client, does not use any caching (which is just what we want for tests).
	client, err = ctrl_client.New(cfg, ctrl_client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create controller-runtime client: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme,
		LeaderElection: false,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize controller manager: %w", err)
	}

	processExecutor = ctrl_testutil.NewTestProcessExecutor()
	dockerOrchestrator := docker.NewDockerCliOrchestrator(ctrl.Log.WithName("DockerCliOrchestrator"), processExecutor)

	exeRunner := exerunners.NewProcessExecutableRunner(processExecutor)
	ideRunner = ctrl_testutil.NewTestIdeRunner()

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
		return nil, fmt.Errorf("failed to initialize Executable reconciler: %w", err)
	}

	execrsR := controllers.NewExecutableReplicaSetReconciler(
		mgr.GetClient(),
		ctrl.Log.WithName("ExecutableReplicaSetReconciler"),
	)
	if err = execrsR.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("failed to initialize ExecutableReplicaSet reconciler: %w", err)
	}

	containerR := controllers.NewContainerReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ContainerReconciler"),
		dockerOrchestrator,
	)
	if err = containerR.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("failed to initialize Container reconciler: %w", err)
	}

	volumeR := controllers.NewVolumeReconciler(
		mgr.GetClient(),
		ctrl.Log.WithName("VolumeReconciler"),
		dockerOrchestrator,
	)
	if err = volumeR.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("failed to initialize ContainerVolume reconciler: %w", err)
	}

	serviceR := controllers.NewServiceReconciler(
		ctx,
		mgr.GetClient(),
		ctrl.Log.WithName("ServiceReconciler"),
		processExecutor,
	)
	if err = serviceR.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("failed to initialize Service reconciler: %w", err)
	}

	if err = controllers.SetupEndpointIndexWithManager(mgr); err != nil {
		return nil, fmt.Errorf("failed to initialize Endpoint index: %w", err)
	}

	// Starts the controller manager and all the associated controllers
	go func() {
		_ = mgr.Start(ctx)
	}()

	return flushFn, nil
}

func waitObjectAssumesState[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](t *testing.T, ctx context.Context, name types.NamespacedName, isInState func(*T) (bool, error)) *T {
	var updatedObject *T = new(T)

	hasExpectedState := func(ctx context.Context) (bool, error) {
		err := client.Get(ctx, name, PT(updatedObject))
		if err != nil {
			t.Fatalf("unable to fetch the object '%s' from API server: %v", name.Name, err)
			return false, err
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
			if errors.IsNotFound(err) {
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

func waitForDockerCommand(t *testing.T, ctx context.Context, command []string, lastArg string) (ctrl_testutil.ProcessExecution, error) {
	command = append([]string{"docker"}, command...)
	pe, err := ctrl_testutil.WaitForCommand(processExecutor, ctx, command, lastArg, (*ctrl_testutil.ProcessExecution).Running)
	return pe, err
}
