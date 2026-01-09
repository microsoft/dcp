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

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/health"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/process"
)

// Starts an test environment for advanced tests that use real process executor and true container orchestrator (Docker or Podman).
// Note that the Executable controller (if included in the mix) only supports process execution (no IDE execution).
func StartAdvancedTestEnvironment(
	ctx context.Context,
	inclCtrl IncludedController,
	instanceTag string,
	testTempDir string,
	log logr.Logger,
) (
	*ctrl_testutil.ApiServerInfo,
	process.Executor,
	error,
) {
	serverInfo, serverErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerUseTrueContainerOrchestrator, log)
	if serverErr != nil {
		return nil, nil, fmt.Errorf("failed to start the API server: %w", serverErr)
	}

	pe := process.NewOSExecutor(log)
	exeRunner := exerunners.NewProcessExecutableRunner(pe)

	managerDone := concurrency.NewAutoResetEvent(true)
	_ = context.AfterFunc(ctx, func() {
		// We are going to stop the API server only after all the controller manager is done.
		// This avoids a bunch of shutdown errors from the manager.
		<-managerDone.Wait()

		serverInfo.Dispose()
		pe.Dispose()
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
			},
			hpSet,
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
		go harvester.Harvest(ctx, serverInfo.ContainerOrchestrator, log.WithName("ResourceCleanup"))

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
				MaxParallelContainerStarts: math.MaxUint8,
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
				ProcessExecutor:               pe,
				AdditionalReconciliationDelay: controllers.TestDelay,
			},
		)
		if err = serviceR.SetupWithManager(mgr, instanceTag+"-ServiceReconciler"); err != nil {
			return nil, nil, fmt.Errorf("failed to initialize Service reconciler: %w", err)
		}
	}

	if inclCtrl&ContainerNetworkTunnelProxyController != 0 {
		tprOpts := controllers.ContainerNetworkTunnelProxyReconcilerConfig{
			Orchestrator:                 serverInfo.ContainerOrchestrator,
			ProcessExecutor:              pe,
			MakeTunnelControlClient:      dcptunproto.NewTunnelControlClient,
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

	return serverInfo, pe, nil
}
