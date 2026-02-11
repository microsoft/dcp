/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrl_manager "sigs.k8s.io/controller-runtime/pkg/manager"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	cmds "github.com/microsoft/dcp/internal/commands"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/internal/dcpclient"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/health"
	"github.com/microsoft/dcp/internal/notifications"
	"github.com/microsoft/dcp/internal/perftrace"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/pkg/kubeconfig"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	ControllerManagerShutdownTimeout = 60 * time.Second
)

var (
	shutdownPerftraceStarted atomic.Bool
)

func NewRunControllersCommand(log logr.Logger) *cobra.Command {
	runControllersCmd := &cobra.Command{
		Use:   "run-controllers",
		Short: "Runs the standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		RunE:  runControllers(log),
		Args:  cobra.NoArgs,
	}

	kubeconfig.EnsureKubeconfigFlag(runControllersCmd.Flags())
	kubeconfig.EnsureKubeconfigPortFlag(runControllersCmd.Flags())

	cmds.AddMonitorFlags(runControllersCmd)
	notifications.AddNotificationSocketFlag(runControllersCmd.Flags())

	return runControllersCmd
}

func getManager(ctx context.Context, log logr.Logger) (ctrl_manager.Manager, error) {
	retryCtx, cancelRetryCtx := context.WithTimeout(ctx, dcpclient.DefaultServerConnectTimeout)
	defer cancelRetryCtx()

	scheme := dcpclient.NewScheme()

	// Depending on the usage pattern, the API server may not be available immediately.
	// Do some retries with exponential back-off before giving up
	mgr, err := resiliency.RetryGetExponential(retryCtx, func() (ctrl_manager.Manager, error) {
		config := ctrlruntime.GetConfigOrDie()
		dcpclient.ApplyDcpOptions(config)
		ctrlMgrOpts := controllers.NewControllerManagerOptions(ctx, scheme, log)
		return ctrlruntime.NewManager(config, ctrlMgrOpts)
	})
	if err != nil {
		log.Error(err, "Unable to create controller manager")
		return nil, err
	}

	// We need to make sure the API server is responding to requests before setting up the controllers
	// as we can get connection refused errors during setup otherwise.
	_, err = resiliency.RetryGetExponential(retryCtx, func() (interface{}, error) {
		var exeList apiv1.ExecutableList
		if listErr := mgr.GetAPIReader().List(retryCtx, &exeList); listErr != nil {
			return nil, listErr
		}

		return nil, nil
	})
	if err != nil {
		log.Error(err, "Unable to confirm the API server is responding")
		return nil, err
	}

	return mgr, nil
}

func runControllers(log logr.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		err := perftrace.CaptureStartupProfileIfRequested(cmd.Context(), log)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

		ctrlCtx, ctrlCtxCancel := cmds.GetMonitorContextFromFlags(cmd.Context(), log)
		defer ctrlCtxCancel()

		_, err = kubeconfig.RequireKubeconfigFlagValue(cmd.Flags())
		if err != nil {
			return fmt.Errorf("cannot set up connection to the API server without kubeconfig file: %w", err)
		}

		trySetupNotificationHandler(ctrlCtx, log)

		mgr, err := getManager(ctrlCtx, log.V(1))
		if err != nil {
			return fmt.Errorf("failed to initialize the controller manager: %w", err)
		}

		processExecutor := process.NewOSExecutor(log)
		defer processExecutor.Dispose()
		containerOrchestrator, orchestratorErr := runtimes.FindAvailableContainerRuntime(ctrlCtx, log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagValue()), processExecutor)
		if orchestratorErr != nil {
			return orchestratorErr
		}
		// Start watching the status of the container orchestrator in the background
		containerOrchestrator.EnsureBackgroundStatusUpdates(ctrlCtx)

		exeRunners := make(map[apiv1.ExecutionType]controllers.ExecutableRunner, 2)
		processRunner := exerunners.NewProcessExecutableRunner(processExecutor)
		exeRunners[apiv1.ExecutionTypeProcess] = processRunner
		ideRunner, err := exerunners.NewIdeExecutableRunner(ctrlCtx, log.WithName("IdeExecutableRunner"))
		if err == nil {
			exeRunners[apiv1.ExecutionTypeIDE] = ideRunner
		}
		// If the IDE runner cannot be created, the details have been logged by the IDE Runner factory function.
		// Executables can still be run, just not via IDE.

		hpSet := health.NewHealthProbeSet(
			ctrlCtx,
			log.WithName("HealthProbeSet"),
			map[apiv1.HealthProbeType]health.HealthProbeExecutor{
				apiv1.HealthProbeTypeHttp: health.NewHttpProbeExecutor(mgr.GetClient(), log.WithName("HttpProbeExecutor")),
			},
		)

		// Run the harvester in a separate goroutine to ensure that it does not block controller startup
		harvester := controllers.NewResourceHarvester()
		go harvester.Harvest(ctrlCtx, containerOrchestrator, log.WithName("ResourceCleanup"))

		const defaultControllerName = ""

		serviceCtrl := controllers.NewServiceReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ServiceReconciler"),
			controllers.ServiceReconcilerConfig{
				ProcessExecutor: processExecutor,
			},
		)
		if err = serviceCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to set up Service controller")
			return err
		}

		if err = controllers.SetupEndpointIndexWithManager(mgr); err != nil {
			log.Error(err, "Unable to set up Endpoint owner index")
			return err
		}

		exCtrl := controllers.NewExecutableReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ExecutableReconciler"),
			exeRunners,
			hpSet,
		)
		if err = exCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to set up Executable controller")
			return err
		}

		exReplicaSetCtrl := controllers.NewExecutableReplicaSetReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ExecutableReplicaSetReconciler"),
		)
		if err = exReplicaSetCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to set up ExecutableReplicaSet controller")
			return err
		}

		containerCtrl := controllers.NewContainerReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ContainerReconciler"),
			containerOrchestrator,
			hpSet,
			controllers.ContainerReconcilerConfig{
				MaxParallelContainerStarts: controllers.DefaultMaxParallelContainerStarts,
			},
		)
		if err = containerCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to set up Container controller")
			return err
		}

		containerExecCtrl := controllers.NewContainerExecReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("ContainerExecReconciler"),
			containerOrchestrator,
		)
		if err = containerExecCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to set up ContainerExec controller")
			return err
		}

		volumeCtrl := controllers.NewVolumeReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("VolumeReconciler"),
			containerOrchestrator,
		)
		if err = volumeCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to set up ContainerVolume controller")
			return err
		}

		networkCtrl := controllers.NewNetworkReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			log.WithName("NetworkReconciler"),
			containerOrchestrator,
			harvester,
		)
		if err = networkCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to setup a ContainerNetwork controller")
			return err
		}

		containerNetworkTunnelProxyCtrl := controllers.NewContainerNetworkTunnelProxyReconciler(
			ctrlCtx,
			mgr.GetClient(),
			mgr.GetAPIReader(),
			controllers.ContainerNetworkTunnelProxyReconcilerConfig{
				Orchestrator:            containerOrchestrator,
				ProcessExecutor:         processExecutor,
				MakeTunnelControlClient: dcptunproto.NewTunnelControlClient,
			},
			log.WithName("TunnelProxyReconciler"),
		)
		if err = containerNetworkTunnelProxyCtrl.SetupWithManager(mgr, defaultControllerName); err != nil {
			log.Error(err, "Unable to setup a ContainerNetworkTunnelProxy controller")
			return err
		}

		mgrRunResultCh := make(chan error, 1)

		// Run the controller manager in a separate goroutine to ensure that the process running controllers
		// can exit even if controller manager Start() method does NOT return in a timely manner
		// after context cancellation (https://github.com/microsoft/usvc/issues/195).
		go func() {
			log.Info("Starting controller manager")
			var mgrRunErr error

			defer func() {
				panicErr := resiliency.MakePanicError(recover(), log)
				if panicErr != nil {
					// Already logged by MakePanicError()
					mgrRunResultCh <- panicErr
				} else if mgrRunErr != nil {
					log.Error(mgrRunErr, "Controller manager failed")
					mgrRunResultCh <- mgrRunErr
				} else {
					log.Info("Controller manager shutting down...")
					mgrRunResultCh <- nil
				}
				close(mgrRunResultCh)
			}()

			mgrRunErr = mgr.Start(ctrlCtx)
		}()

		<-ctrlCtx.Done()

		select {
		case mgrRunErr := <-mgrRunResultCh:
			return mgrRunErr
		case <-time.After(ControllerManagerShutdownTimeout):
			mgrShutdownErr := fmt.Errorf("controller manager did not shut down in a timely manner, exiting anyway...")
			log.Error(mgrShutdownErr, "")
			return mgrShutdownErr
		}
	}
}

func trySetupNotificationHandler(notifyCtx context.Context, log logr.Logger) {
	notifySocketPath := notifications.GetNotificationSocketPath()
	if notifySocketPath == "" {
		return
	}

	log.V(1).Info("Setting up notification receiver", "SocketPath", notifySocketPath)

	_, nrErr := notifications.NewNotificationSubscription(notifyCtx, notifySocketPath, log.WithName("NotificationReceiver"), func(n notifications.Notification) {
		handleNotification(notifyCtx, n, log)
	})
	if nrErr != nil {
		log.Error(nrErr, "Failed to create cleanup notification receiver")
	}
}

func handleNotification(ctx context.Context, note notifications.Notification, log logr.Logger) {
	switch note.Kind() {

	case notifications.NotificationKindCleanupStarted:
		log.V(1).Info("Received cleanup notification, suppressing TCP stream completion errors...")
		proxy.SilenceTcpStreamCompletionErrors.Store(true)
		if !shutdownPerftraceStarted.Swap(true) {
			log.V(1).Info("Attempting to start shutdown profiling")
			if profileErr := perftrace.CaptureShutdownProfileIfRequested(ctx, log); profileErr != nil {
				log.Error(profileErr, "Could not start shutdown profiling")
				// Best effort--do not fail the request if we cannot start profiling.
			}
		}

	case notifications.NotificationKindPerftraceRequest:
		perfTraceReq, ok := note.(*notifications.PerftraceRequestNotification)
		if !ok {
			log.Error(fmt.Errorf("invalid perfomance trace request"), "Unable to collect performance trace")
			return
		}

		profileCtx, profileCtxCancel := context.WithTimeout(ctx, perfTraceReq.Duration)
		profileErr := perftrace.StartProfiling(profileCtx, profileCtxCancel, perftrace.ProfileTypeSnapshot, log)
		if profileErr != nil {
			log.Error(profileErr, "Could not start performance profiling")
			// Best effort--do not fail the request if we cannot start profiling.
		}
	}
}
