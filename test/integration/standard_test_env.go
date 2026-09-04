/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpproc"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/health"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// TestEnvironmentInfo provides information about the test environment created via StartTestEnvironment().
type TestEnvironmentInfo struct {
	*internal_testutil.TestProcessExecutor
	*ctrl_testutil.TestProcessExecutableRunner
	*ctrl_testutil.TestIdeRunner
	*ctrl_testutil.TestTunnelControlClient
	TerminalProcessFactoryDispatcher *ctrl_testutil.TerminalProcessFactoryDispatcher
	ContainerAttachFactoryDispatcher *ctrl_testutil.ContainerAttachFactoryDispatcher
	StateStore                       *statestore.Store
	ResourceLeaseOwner               process.ProcessHandle
	Log                              logr.Logger
}

type TestEnvironmentOptions struct {
	WorkloadID                    commonapi.WorkloadID
	DecorateContainerOrchestrator func(containers.ContainerOrchestrator, *statestore.Store) containers.ContainerOrchestrator
}

// Starts the DCP API server (separate process) and standard controllers (in-proc).
func StartTestEnvironment(
	ctx context.Context,
	inclCtrl IncludedController,
	instanceTag string,
	testTempDir string,
) (
	*ctrl_testutil.ApiServerInfo,
	*TestEnvironmentInfo,
	error,
) {
	return StartTestEnvironmentWithOptions(ctx, inclCtrl, instanceTag, testTempDir, TestEnvironmentOptions{})
}

func StartTestEnvironmentWithOptions(
	ctx context.Context,
	inclCtrl IncludedController,
	instanceTag string,
	testTempDir string,
	options TestEnvironmentOptions,
) (
	*ctrl_testutil.ApiServerInfo,
	*TestEnvironmentInfo,
	error,
) {
	if inclCtrl&ContainerNetworkTunnelProxyController != 0 {
		inclCtrl |= NamespaceController | PhysicalContainerImageController | PhysicalContainerController
	}

	sessionFolder, sessionFolderErr := testutil.CreateTestSessionDir()
	if sessionFolderErr != nil {
		return nil, nil, fmt.Errorf("failed to create session folder for API server instance: %w", sessionFolderErr)
	}

	log := testutil.NewLogWithResourceSinkForTesting(instanceTag, sessionFolder)
	ctrl.SetLogger(log)

	serverInfo, serverErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerFlagsNone, log, sessionFolder)
	if serverErr != nil {
		return nil, nil, fmt.Errorf("failed to start the API server: %w", serverErr)
	}

	stateStore, stateStoreCleanup, stateStoreErr := createTestStateStore(ctx, testTempDir)
	if stateStoreErr != nil {
		serverInfo.Dispose()
		return nil, nil, fmt.Errorf("failed to initialize state store: %w", stateStoreErr)
	}
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	if leaseOwnerErr != nil {
		serverInfo.Dispose()
		stateStoreCleanup()
		return nil, nil, fmt.Errorf("failed to initialize state store lease owner identity: %w", leaseOwnerErr)
	}
	if options.DecorateContainerOrchestrator != nil {
		decoratedOrchestrator := options.DecorateContainerOrchestrator(serverInfo.ContainerOrchestrator, stateStore)
		if decoratedOrchestrator == nil {
			serverInfo.Dispose()
			stateStoreCleanup()
			return nil, nil, fmt.Errorf("container orchestrator decorator returned nil")
		}
		serverInfo.ContainerOrchestrator = decoratedOrchestrator
	}
	pex := internal_testutil.NewTestProcessExecutor(ctx)
	// On Windows the process Executable runner uses the dcp stop-process-tree subcommand, so we need to simulate that.
	pex.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"dcp", "stop-process-tree"},
		},
		RunCommand: dcpproc.SimulateStopProcessTreeCommand,
	})

	exeRunner := ctrlutil.NewTestProcessExecutableRunner(pex)
	terminalDispatcher := ctrl_testutil.NewTerminalProcessFactoryDispatcher(exeRunner)
	ir := ctrl_testutil.NewTestIdeRunner(ctx)

	// Wire a container attach dispatcher onto the TestContainerOrchestrator if
	// the API server created one. With a real container orchestrator (e.g. in
	// advanced_test_env.go) this stays nil; per-test container terminal tests
	// must request the standard environment to use this hook.
	var containerAttachDispatcher *ctrl_testutil.ContainerAttachFactoryDispatcher
	if tco, ok := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator); ok {
		containerAttachDispatcher = ctrl_testutil.NewContainerAttachFactoryDispatcher(tco)
	}

	// This is initially set to allow quick and clean shutdown if some of the initialization code below fails,
	// but we will reset when the manager starts.
	managerDone := concurrency.NewAutoResetEvent(true)

	_ = context.AfterFunc(ctx, func() {
		// We are going to stop the API server only after all the controller manager is done.
		// This avoids a bunch of shutdown errors from the manager.
		<-managerDone.Wait()

		tpeCloseErr := pex.Close()
		if tpeCloseErr != nil {
			log.Error(tpeCloseErr, "Failed to close the test process executor")
		}

		stateStoreCleanup()
		serverInfo.Dispose()
	})

	opts := controllers.NewControllerManagerOptions(ctx, serverInfo.Client.Scheme(), log)
	mgr, err := ctrl.NewManager(serverInfo.ClientConfig, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize controller manager: %w", err)
	}

	hpSet := health.NewHealthProbeSet(
		ctx,
		log.WithName("HealthProbeSet"),
		map[apiv1.HealthProbeType]health.HealthProbeExecutor{
			apiv1.HealthProbeTypeHttp: health.NewHttpProbeExecutor(mgr.GetClient(), log.WithName("HttpProbeExecutor")),
		},
	)

	if inclCtrl&NamespaceController != 0 {
		namespaceR := controllers.NewNamespaceReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("NamespaceReconciler"),
		)
		if err = namespaceR.SetupWithManager(mgr, instanceTag+"-NamespaceReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Namespace reconciler: %w", err)
		}
	}

	if inclCtrl&ExecutableController != 0 {
		execR := controllers.NewExecutableReconcilerWithConfig(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ExecutableReconciler"),
			map[apiv1.ExecutionType]controllers.ExecutableRunner{
				apiv1.ExecutionTypeProcess: exeRunner,
				apiv1.ExecutionTypeIDE:     ir,
			},
			hpSet,
			controllers.ExecutableReconcilerConfig{
				StateStore:         stateStore,
				ResourceLeaseOwner: leaseOwner,
				WorkloadID:         options.WorkloadID,
			},
		)
		if err = execR.SetupWithManager(mgr, instanceTag+"-ExecutableReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Executable reconciler: %w", err)
		}
	}

	if inclCtrl&ExecutableReplicaSetController != 0 {
		execrsR := controllers.NewExecutableReplicaSetReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ExecutableReplicaSetReconciler"),
		)
		if err = execrsR.SetupWithManager(mgr, instanceTag+"-ExecutableReplicaSetReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize ExecutableReplicaSet reconciler: %w", err)
		}
	}

	if inclCtrl&NetworkController != 0 {
		// Run the harvester in a separate goroutine to ensure that it does not block controller startup
		harvester := controllers.NewResourceHarvester()
		go harvester.MockHarvest(ctx, 2*time.Second, log.WithName("ResourceCleanup"))

		networkR := controllers.NewNetworkReconcilerWithConfig(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("NetworkReconciler"),
			serverInfo.ContainerOrchestrator,
			harvester,
			controllers.NetworkReconcilerConfig{
				StateStore:         stateStore,
				ResourceLeaseOwner: leaseOwner,
				WorkloadID:         options.WorkloadID,
			},
		)
		if err = networkR.SetupWithManager(mgr, instanceTag+"-NetworkReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Network reconciler: %w", err)
		}
	}

	if inclCtrl&ContainerController != 0 {
		containerR := controllers.NewContainerReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ContainerReconciler"),
			serverInfo.ContainerOrchestrator,
			hpSet,
			controllers.ContainerReconcilerConfig{
				MaxParallelContainerStarts:      math.MaxUint8,
				ContainerStartupTimeoutOverride: 2 * time.Second,
				StateStore:                      stateStore,
				ResourceLeaseOwner:              leaseOwner,
				ProcessExecutor:                 pex,
				WorkloadID:                      options.WorkloadID,
			},
		)
		if err = containerR.SetupWithManager(mgr, instanceTag+"-ContainerReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Container reconciler: %w", err)
		}
	}

	if inclCtrl&PhysicalContainerImageController != 0 {
		physicalContainerImageR := controllers.NewPhysicalContainerImageReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("PhysicalContainerImageReconciler"),
			serverInfo.ContainerOrchestrator,
		)
		if err = physicalContainerImageR.SetupWithManager(mgr, instanceTag+"-PhysicalContainerImageReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize PhysicalContainerImage reconciler: %w", err)
		}
	}

	if inclCtrl&PhysicalContainerController != 0 {
		physicalContainerR := controllers.NewPhysicalContainerReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("PhysicalContainerReconciler"),
			serverInfo.ContainerOrchestrator,
		)
		if err = physicalContainerR.SetupWithManager(mgr, instanceTag+"-PhysicalContainerReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize PhysicalContainer reconciler: %w", err)
		}
	}

	if inclCtrl&PhysicalContainerNetworkController != 0 {
		physicalContainerNetworkR := controllers.NewPhysicalContainerNetworkReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("PhysicalContainerNetworkReconciler"),
			serverInfo.ContainerOrchestrator,
		)
		if err = physicalContainerNetworkR.SetupWithManager(mgr, instanceTag+"-PhysicalContainerNetworkReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize PhysicalContainerNetwork reconciler: %w", err)
		}
	}

	if inclCtrl&PhysicalContainerVolumeController != 0 {
		physicalContainerVolumeR := controllers.NewPhysicalContainerVolumeReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("PhysicalContainerVolumeReconciler"),
			serverInfo.ContainerOrchestrator,
		)
		if err = physicalContainerVolumeR.SetupWithManager(mgr, instanceTag+"-PhysicalContainerVolumeReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize PhysicalContainerVolume reconciler: %w", err)
		}
	}

	if inclCtrl&ContainerExecController != 0 {
		containerExecR := controllers.NewContainerExecReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ContainerExecReconciler"),
			serverInfo.ContainerOrchestrator,
		)
		if err = containerExecR.SetupWithManager(mgr, instanceTag+"-ContainerExecReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize ContainerExec reconciler: %w", err)
		}
	}

	if inclCtrl&VolumeController != 0 {
		volumeR := controllers.NewVolumeReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("VolumeReconciler"),
			serverInfo.ContainerOrchestrator,
			controllers.VolumeReconcilerConfig{
				StateStore:         stateStore,
				ResourceLeaseOwner: leaseOwner,
				WorkloadID:         options.WorkloadID,
			},
		)
		if err = volumeR.SetupWithManager(mgr, instanceTag+"-VolumeReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize ContainerVolume reconciler: %w", err)
		}
	}

	if inclCtrl&ServiceController != 0 {
		serviceR := controllers.NewServiceReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ServiceReconciler"),
			controllers.ServiceReconcilerConfig{
				ProcessExecutor:               pex,
				CreateProxy:                   ctrl_testutil.NewTestProxy,
				AdditionalReconciliationDelay: controllers.TestDelay,
			},
		)
		if err = serviceR.SetupWithManager(mgr, instanceTag+"-ServiceReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Service reconciler: %w", err)
		}
	}

	var tcc *ctrl_testutil.TestTunnelControlClient

	if inclCtrl&ContainerNetworkTunnelProxyController != 0 {
		tcc = ctrl_testutil.NewTestTunnelControlClient()
		tprOpts := controllers.ContainerNetworkTunnelProxyReconcilerConfig{
			Orchestrator:                 serverInfo.ContainerOrchestrator,
			ProcessExecutor:              pex,
			MakeTunnelControlClient:      func(_ grpc.ClientConnInterface) dcptunproto.TunnelControlClient { return tcc },
			MaxTunnelPreparationAttempts: 2,
		}

		if testTempDir != NoSeparateWorkingDir {
			tprOpts.MostRecentImageBuildsFilePath = filepath.Join(testTempDir, instanceTag+".imglist")
		}

		tunnelProxyR := controllers.NewContainerNetworkTunnelProxyReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			tprOpts,
			log.WithName("TunnelProxyReconciler"),
		)
		if err = tunnelProxyR.SetupWithManager(mgr, instanceTag+"-ContainerNetworkTunnelProxyReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize ContainerNetworkTunnelProxy reconciler: %w", err)
		}
	}

	if err = controllers.SetupEndpointIndexWithManager(mgr); err != nil {
		return nil, nil, fmt.Errorf("failed to initialize Endpoint index: %w", err)
	}

	// Starts the controller manager and all the associated controllers
	managerDone.Clear()
	go func() {
		_ = mgr.Start(ctx)
		managerDone.Set()
	}()

	teInfo := &TestEnvironmentInfo{
		TestProcessExecutor:              pex,
		TestProcessExecutableRunner:      exeRunner,
		TestIdeRunner:                    ir,
		TestTunnelControlClient:          tcc,
		TerminalProcessFactoryDispatcher: terminalDispatcher,
		ContainerAttachFactoryDispatcher: containerAttachDispatcher,
		StateStore:                       stateStore,
		ResourceLeaseOwner:               leaseOwner,
		Log:                              log,
	}
	return serverInfo, teInfo, nil
}
