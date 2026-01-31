/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcpproc"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/health"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/concurrency"
)

// TestEnvironmentInfo provides information about the test environment created via StartTestEnvironment().
type TestEnvironmentInfo struct {
	*internal_testutil.TestProcessExecutor
	*ctrl_testutil.TestProcessExecutableRunner
	*ctrl_testutil.TestIdeRunner
	*ctrl_testutil.TestTunnelControlClient
}

// Starts the DCP API server (separate process) and standard controllers (in-proc).
func StartTestEnvironment(
	ctx context.Context,
	inclCtrl IncludedController,
	instanceTag string,
	testTempDir string,
	log logr.Logger,
) (
	*ctrl_testutil.ApiServerInfo,
	*TestEnvironmentInfo,
	error,
) {
	serverInfo, serverErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerFlagsNone, log)
	if serverErr != nil {
		return nil, nil, fmt.Errorf("failed to start the API server: %w", serverErr)
	}

	pex := internal_testutil.NewTestProcessExecutor(ctx)
	// On Windows the process Executable runner uses the dcp stop-process-tree subcommand, so we need to simulate that.
	pex.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{os.Args[0], "stop-process-tree"},
		},
		RunCommand: dcpproc.SimulateStopProcessTreeCommand,
	})

	exeRunner := ctrlutil.NewTestProcessExecutableRunner(pex)
	ir := ctrl_testutil.NewTestIdeRunner(ctx)

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

	if inclCtrl&ExecutableController != 0 {
		execR := controllers.NewExecutableReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ExecutableReconciler"),
			map[apiv1.ExecutionType]controllers.ExecutableRunner{
				apiv1.ExecutionTypeProcess: exeRunner,
				apiv1.ExecutionTypeIDE:     ir,
			},
			hpSet,
			nil, // debugSessions
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

		networkR := controllers.NewNetworkReconciler(
			ctx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("NetworkReconciler"),
			serverInfo.ContainerOrchestrator,
			harvester,
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
			},
		)
		if err = containerR.SetupWithManager(mgr, instanceTag+"-ContainerReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Container reconciler: %w", err)
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
			Orchestrator:                    serverInfo.ContainerOrchestrator,
			ProcessExecutor:                 pex,
			MakeTunnelControlClient:         func(_ grpc.ClientConnInterface) dcptunproto.TunnelControlClient { return tcc },
			MaxTunnelPreparationAttempts:    2,
			ContainerStartupTimeoutOverride: 2 * time.Second,
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
		TestProcessExecutor:         pex,
		TestProcessExecutableRunner: exeRunner,
		TestIdeRunner:               ir,
		TestTunnelControlClient:     tcc,
	}
	return serverInfo, teInfo, nil
}
