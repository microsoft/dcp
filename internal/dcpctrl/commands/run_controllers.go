package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrl_manager "sigs.k8s.io/controller-runtime/pkg/manager"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/docker"
	"github.com/microsoft/usvc-apiserver/internal/exerunners"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
)

var (
	scheme = apiruntime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1.AddToScheme(scheme))
}

func NewRunControllersCommand(logger logger.Logger) *cobra.Command {
	runControllersCmd := &cobra.Command{
		Use:   "run-controllers",
		Short: "Runs the standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		RunE:  runControllers(logger),
		Args:  cobra.NoArgs,
	}

	kubeconfig.EnsureKubeconfigFlag(runControllersCmd.Flags())

	return runControllersCmd
}

func getManager(ctx context.Context, log logr.Logger) (ctrl_manager.Manager, error) {
	retryCtx, cancelRetryCtx := context.WithTimeout(ctx, 15*time.Second)
	defer cancelRetryCtx()

	// Depending on the usage pattern, the API server may not be available immediately.
	// Do some retries with exponential back-off before giving up
	mgr, err := resiliency.RetryGet(retryCtx, func() (ctrl_manager.Manager, error) {
		return ctrlruntime.NewManager(ctrlruntime.GetConfigOrDie(), ctrlruntime.Options{
			Scheme:             scheme,
			LeaderElection:     false,
			MetricsBindAddress: "0",
			Logger:             log.WithName("ControllerManager"),
		})
	})
	if err != nil {
		log.Error(err, "unable to create controller manager")
		return nil, err
	}

	// We need to make sure the API server is responding to requests before setting up the controllers
	// as we can get connection refuesed errors during setup otherwise.
	if _, err := resiliency.RetryGet(retryCtx, func() (interface{}, error) {
		var exeList apiv1.ExecutableList
		if err := mgr.GetAPIReader().List(retryCtx, &exeList); err != nil {
			return nil, err
		}

		return nil, nil
	}); err != nil {
		log.Error(err, "unable to confirm the API server is responding")
		return nil, err
	}

	return mgr, nil
}

func runControllers(logger logger.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		// ctlrruntime.SetLogger() was already called by main()
		log := logger.WithName("dcpctrl")

		_, err := kubeconfig.EnsureKubeconfigFlagValue(cmd.Flags())
		if err != nil {
			return fmt.Errorf("cannot set up connection to the API server without kubeconfig file: %w", err)
		}

		mgr, err := getManager(cmd.Context(), log.V(1))
		if err != nil {
			return fmt.Errorf("failed to initialize the controller manager: %w", err)
		}

		processExecutor := process.NewOSExecutor()
		containerOrchestrator := docker.NewDockerCliOrchestrator(log.WithName("DockerOrchestrator"), processExecutor)
		exeRunners := make(map[apiv1.ExecutionType]controllers.ExecutableRunner, 2)
		processRunner := exerunners.NewProcessExecutableRunner(processExecutor)
		exeRunners[apiv1.ExecutionTypeProcess] = processRunner
		ideRunner, err := exerunners.NewIdeExecutableRunner(log.WithName("IdeExecutableRunner"))
		if err == nil {
			exeRunners[apiv1.ExecutionTypeIDE] = ideRunner
		}
		// If the IDE runner cannot be created, the details have been logged by the IDE Runner factory function.
		// Executables can still be run, just not via IDE.

		exCtrl := controllers.NewExecutableReconciler(
			mgr.GetClient(),
			log.WithName("ExecutableReconciler"),
			exeRunners,
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
			cmd.Context(),
			mgr.GetClient(),
			log.WithName("ContainerReconciler"),
			containerOrchestrator,
		)
		if err = containerCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Container controller")
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

		serviceCtrl := controllers.NewServiceReconciler(
			mgr.GetClient(),
			log.WithName("ServiceReconciler"),
			processExecutor,
		)
		if err = serviceCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Service controller")
			return err
		}

		endpointCtrl := controllers.NewEndpointReconciler(
			mgr.GetClient(),
			log.WithName("EndpointReconciler"),
		)
		if err = endpointCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Endpoint controller")
			return err
		}

		log.Info("starting controller manager")
		err = mgr.Start(cmd.Context())
		if err != nil {
			log.Error(err, "contoller manager failed")
			return err
		}

		log.Info("controller manager shutting down...")
		return nil
	}
}
