package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrl_manager "sigs.k8s.io/controller-runtime/pkg/manager"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/containers/runtimes"
	"github.com/microsoft/usvc-apiserver/internal/dcpclient"
	"github.com/microsoft/usvc-apiserver/internal/exerunners"
	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

func NewRunControllersCommand(logger logger.Logger) *cobra.Command {
	runControllersCmd := &cobra.Command{
		Use:   "run-controllers",
		Short: "Runs the standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		RunE:  runControllers(logger),
		Args:  cobra.NoArgs,
	}

	kubeconfig.EnsureKubeconfigFlag(runControllersCmd.Flags())
	kubeconfig.EnsureKubeconfigPortFlag(runControllersCmd.Flags())

	cmds.AddMonitorFlags(runControllersCmd)

	return runControllersCmd
}

func getManager(ctx context.Context, log logr.Logger) (ctrl_manager.Manager, error) {
	retryCtx, cancelRetryCtx := context.WithTimeout(ctx, 15*time.Second)
	defer cancelRetryCtx()

	scheme := dcpclient.NewScheme()

	// Depending on the usage pattern, the API server may not be available immediately.
	// Do some retries with exponential back-off before giving up
	mgr, err := resiliency.RetryGetExponential(retryCtx, func() (ctrl_manager.Manager, error) {
		config := ctrlruntime.GetConfigOrDie()
		token, _ := os.LookupEnv(kubeconfig.DCP_SECURE_TOKEN)
		if token != "" {
			// If a token was supplied, use it to authenticate to the API server
			config.BearerToken = token
		}
		ctrlMgrOpts := controllers.NewControllerManagerOptions(ctx, scheme, log)
		return ctrlruntime.NewManager(config, ctrlMgrOpts)
	})
	if err != nil {
		log.Error(err, "unable to create controller manager")
		return nil, err
	}

	// We need to make sure the API server is responding to requests before setting up the controllers
	// as we can get connection refuesed errors during setup otherwise.
	_, err = resiliency.RetryGetExponential(retryCtx, func() (interface{}, error) {
		var exeList apiv1.ExecutableList
		if listErr := mgr.GetAPIReader().List(retryCtx, &exeList); listErr != nil {
			return nil, listErr
		}

		return nil, nil
	})
	if err != nil {
		log.Error(err, "unable to confirm the API server is responding")
		return nil, err
	}

	return mgr, nil
}

func runControllers(logger logger.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		// ctlrruntime.SetLogger() was already called by main()
		log := logger.WithName("dcpctrl")

		err := perftrace.CaptureStartupProfileIfRequested(cmd.Context(), log)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

		ctx := cmds.TryGetMonitorContext(cmd.Context(), log.WithName("monitor"))

		_, err = kubeconfig.RequireKubeconfigFlagValue(cmd.Flags())
		if err != nil {
			return fmt.Errorf("cannot set up connection to the API server without kubeconfig file: %w", err)
		}

		mgr, err := getManager(ctx, log.V(1))
		if err != nil {
			return fmt.Errorf("failed to initialize the controller manager: %w", err)
		}

		processExecutor := process.NewOSExecutor(log)
		containerOrchestrator, orchestratorErr := runtimes.FindAvailableContainerRuntime(ctx, log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagValue()), processExecutor)
		if orchestratorErr != nil {
			return orchestratorErr
		}
		// Start watching the status of the container orchestrator in the background
		containerOrchestrator.EnsureBackgroundStatusUpdates(ctx)

		exeRunners := make(map[apiv1.ExecutionType]controllers.ExecutableRunner, 2)
		processRunner := exerunners.NewProcessExecutableRunner(processExecutor)
		exeRunners[apiv1.ExecutionTypeProcess] = processRunner
		ideRunner, err := exerunners.NewIdeExecutableRunner(ctx, log.WithName("IdeExecutableRunner"))
		if err == nil {
			exeRunners[apiv1.ExecutionTypeIDE] = ideRunner
		}
		// If the IDE runner cannot be created, the details have been logged by the IDE Runner factory function.
		// Executables can still be run, just not via IDE.

		hpSet := health.NewHealthProbeSet(
			ctx,
			log.WithName("HealthProbeSet"),
			map[apiv1.HealthProbeType]health.HealthProbeExecutor{
				apiv1.HealthProbeTypeHttp: health.HealthProbeExecutorFunc(health.ExecuteHttpProbe),
			},
		)

		exCtrl := controllers.NewExecutableReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ExecutableReconciler"),
			exeRunners,
			hpSet,
		)
		if err = exCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Executable controller")
			return err
		}

		exReplicaSetCtrl := controllers.NewExecutableReplicaSetReconciler(
			mgr.GetClient(),
			log.WithName("ExecutableReplicaSetReconciler"),
		)
		if err = exReplicaSetCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up ExecutableReplicaSet controller")
			return err
		}

		containerCtrl := controllers.NewContainerReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ContainerReconciler"),
			containerOrchestrator,
		)
		if err = containerCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Container controller")
			return err
		}

		containerExecCtrl := controllers.NewContainerExecReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ContainerExecReconciler"),
			containerOrchestrator,
		)
		if err = containerExecCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up ContainerExec controller")
			return err
		}

		volumeCtrl := controllers.NewVolumeReconciler(
			mgr.GetClient(),
			log.WithName("VolumeReconciler"),
			containerOrchestrator,
		)
		if err = volumeCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up ContainerVolume controller")
			return err
		}

		networkCtrl := controllers.NewNetworkReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("NetworkReconciler"),
			containerOrchestrator,
		)
		if err = networkCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to setup a ContainerNetwork controller")
			return err
		}

		serviceCtrl := controllers.NewServiceReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ServiceReconciler"),
			processExecutor,
		)
		if err = serviceCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Service controller")
			return err
		}

		if err = controllers.SetupEndpointIndexWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Endpoint owner index")
			return err
		}

		log.Info("starting controller manager")
		err = mgr.Start(ctx)
		if err != nil {
			log.Error(err, "contoller manager failed")
			return err
		}

		log.Info("controller manager shutting down...")
		return nil
	}
}
