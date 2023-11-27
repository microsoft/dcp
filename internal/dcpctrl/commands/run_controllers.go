package commands

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrl_manager "sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/docker"
	"github.com/microsoft/usvc-apiserver/internal/exerunners"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/telemetry"
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
	kubeconfig.EnsureKubeconfigPortFlag(runControllersCmd.Flags())

	cmds.AddMonitorFlags(runControllersCmd)

	return runControllersCmd
}

func getManager(ctx context.Context, log logr.Logger) (ctrl_manager.Manager, error) {
	retryCtx, cancelRetryCtx := context.WithTimeout(ctx, 15*time.Second)
	defer cancelRetryCtx()

	// Depending on the usage pattern, the API server may not be available immediately.
	// Do some retries with exponential back-off before giving up
	mgr, err := resiliency.RetryGet(retryCtx, func() (ctrl_manager.Manager, error) {
		return ctrlruntime.NewManager(ctrlruntime.GetConfigOrDie(), ctrlruntime.Options{
			Scheme:         scheme,
			LeaderElection: false,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
			Logger: log.WithName("ControllerManager"),
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

		err := perftrace.CaptureStartupProfileIfRequested(cmd.Context(), log)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

		ctx := cmds.Monitor(cmd.Context(), log.WithName("monitor"))

		_, err = kubeconfig.EnsureKubeconfigFlagValue(cmd.Flags())
		if err != nil {
			return fmt.Errorf("cannot set up connection to the API server without kubeconfig file: %w", err)
		}

		mgr, err := getManager(ctx, log.V(1))
		if err != nil {
			return fmt.Errorf("failed to initialize the controller manager: %w", err)
		}

		processExecutor := process.NewOSExecutor()
		containerOrchestrator := docker.NewDockerCliOrchestrator(log.WithName("DockerOrchestrator"), processExecutor)
		exeRunners := make(map[apiv1.ExecutionType]controllers.ExecutableRunner, 2)
		processRunner := exerunners.NewProcessExecutableRunner(processExecutor)
		exeRunners[apiv1.ExecutionTypeProcess] = processRunner
		ideRunner, err := exerunners.NewIdeExecutableRunner(ctx, log.WithName("IdeExecutableRunner"))
		if err == nil {
			exeRunners[apiv1.ExecutionTypeIDE] = ideRunner
		}
		// If the IDE runner cannot be created, the details have been logged by the IDE Runner factory function.
		// Executables can still be run, just not via IDE.

		exCtrlInner := controllers.NewExecutableReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ExecutableReconciler"),
			exeRunners,
		)
		exCtrl := newReconcilerWithTelemetry(exCtrlInner)
		if err = exCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Executable controller")
			return err
		}

		exReplicaSetCtrlInner := controllers.NewExecutableReplicaSetReconciler(
			mgr.GetClient(),
			log.WithName("ExecutableReplicaSetReconciler"),
		)
		exReplicaSetCtrl := newReconcilerWithTelemetry(exReplicaSetCtrlInner)
		if err = exReplicaSetCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up ExecutableReplicaSet controller")
			return err
		}

		containerCtrlInner := controllers.NewContainerReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ContainerReconciler"),
			containerOrchestrator,
		)
		containerCtrl := newReconcilerWithTelemetry(containerCtrlInner)
		if err = containerCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up Container controller")
			return err
		}

		volumeCtrlInner := controllers.NewVolumeReconciler(
			mgr.GetClient(),
			log.WithName("VolumeReconciler"),
			containerOrchestrator,
		)
		volumeCtrl := newReconcilerWithTelemetry(volumeCtrlInner)
		if err = volumeCtrl.SetupWithManager(mgr); err != nil {
			log.Error(err, "unable to set up ContainerVolume controller")
			return err
		}

		serviceCtrlInner := controllers.NewServiceReconciler(
			ctx,
			mgr.GetClient(),
			log.WithName("ServiceReconciler"),
			processExecutor,
		)
		serviceCtrl := newReconcilerWithTelemetry(serviceCtrlInner)
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

type reconcilerWithSetupWithManager interface {
	reconcile.Reconciler
	SetupWithManager(ctrl_manager.Manager) error
}

type reconcilerWithTelemetry struct {
	inner  reconcilerWithSetupWithManager
	tracer trace.Tracer
}

func newReconcilerWithTelemetry(inner reconcilerWithSetupWithManager) reconcilerWithTelemetry {
	controllerName := reflect.TypeOf(inner).String()
	tracer := otel.GetTracerProvider().Tracer(controllerName)
	return reconcilerWithTelemetry{
		inner:  inner,
		tracer: tracer,
	}
}

func (r reconcilerWithTelemetry) Reconcile(parentCtx context.Context, req reconcile.Request) (reconcile.Result, error) {
	return telemetry.CallWithTelemetryAndErrorHandling(r.tracer, "Reconcile", parentCtx, func(ctx context.Context) (reconcile.Result, error) { return r.inner.Reconcile(ctx, req) })
}

func (r reconcilerWithTelemetry) SetupWithManager(mgr ctrl_manager.Manager) error {
	return r.inner.SetupWithManager(mgr)
}
