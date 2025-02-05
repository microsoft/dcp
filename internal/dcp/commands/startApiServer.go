package commands

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	"github.com/microsoft/usvc-apiserver/internal/appmgmt"
	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/dcp/bootstrap"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	rootDir    string
	detach     bool
	serverOnly bool
)

func NewStartApiSrvCommand(log logger.Logger) (*cobra.Command, error) {
	startApiSrvCmd := &cobra.Command{
		Use:    "start-apiserver",
		Short:  "Starts the API server and controllers, but does not attempt to run any application",
		RunE:   startApiSrv(log),
		Args:   cobra.NoArgs,
		Hidden: true, // This command is mostly for testing
	}

	kubeconfig.EnsureKubeconfigFlag(startApiSrvCmd.Flags())
	kubeconfig.EnsureKubeconfigPortFlag(startApiSrvCmd.Flags())

	startApiSrvCmd.Flags().StringVarP(&rootDir, "root-dir", "r", "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")
	startApiSrvCmd.Flags().BoolVar(&detach, "detach", false, "If present, instructs DCP to fork itself as a detached process.")
	startApiSrvCmd.Flags().BoolVar(&serverOnly, "server-only", false, "If present, instructs DCP to start only the API server and not the controllers. This is useful for testing the API server in isolation.")

	container_flags.EnsureRuntimeFlag(startApiSrvCmd.Flags())
	container_flags.EnsureTestContainerLogSourceFlag(startApiSrvCmd.Flags())
	cmds.AddMonitorFlags(startApiSrvCmd)

	return startApiSrvCmd, nil
}

func startApiSrv(log logger.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		log := log.WithName("start-apiserver")

		apiServerCtx, apiServerCtxCancel := cmds.GetMonitorContextFromFlags(cmd.Context(), log.WithName("monitor"))
		defer apiServerCtxCancel()

		if detach {
			args := make([]string, 0, len(os.Args)-2)

			hasContainerRuntimeFlag := false
			for _, arg := range os.Args[1:] {
				if arg != "--detach" {
					args = append(args, arg)
				}

				if arg == container_flags.GetRuntimeFlag() {
					hasContainerRuntimeFlag = true
				}
			}

			if !hasContainerRuntimeFlag {
				args = append(args, container_flags.GetRuntimeFlag(), string(container_flags.GetRuntimeFlagValue()))
			}

			log.V(1).Info("Forking command", "cmd", os.Args[0], "args", args)

			usvc_io.PreserveSessionFolder() // The forked process will take care of cleaning up the session folder

			forked := exec.Command(os.Args[0], args...)
			process.ForkFromParent(forked)

			if err := forked.Start(); err != nil {
				log.Error(err, "forked process failed to run")
				return err
			} else {
				log.V(1).Info("Forked process started", "pid", forked.Process.Pid)
			}

			if err := forked.Process.Release(); err != nil {
				log.Error(err, "release failed for process", "pid", forked.Process.Pid)
				return err
			}

			return nil
		}

		err := perftrace.CaptureStartupProfileIfRequested(apiServerCtx, log)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

		if rootDir == "" {
			rootDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("could not determine the working directory: %w", err)
			}
		}

		kconfig, err := kubeconfig.EnsureKubeconfigData(cmd.Flags(), log)
		if err != nil {
			return err
		}

		var allExtensions []bootstrap.DcpExtension
		if !serverOnly {
			allExtensions, err = bootstrap.GetExtensions(apiServerCtx, log)
			if err != nil {
				return err
			}
		}

		runEvtHandlers := bootstrap.DcpRunEventHandlers{
			BeforeApiSrvShutdown: func(requestedResourceCleanup apiserver.ApiServerResourceCleanup) error {
				// If we are in server-only mode (no standard controllers) such as when running tests,
				// there is no point trying to clean up all resources on shutdown because no actual resources are involved,
				// it is all test mocks. Another case to avoid full cleanup is when shutdown request explicitly disables it.
				if serverOnly || !requestedResourceCleanup.IsFull() {
					return nil
				}

				cleanupErr := appmgmt.CleanupAllResources(log)
				return cleanupErr // Already logged by appmgmt.CleanupAllResources()
			},
		}

		invocationFlags := []string{
			"--kubeconfig", kconfig.Path(),
			container_flags.GetRuntimeFlag(), string(container_flags.GetRuntimeFlagValue()),
		}
		if verbosityArg := logger.GetVerbosityArg(cmd.Flags()); verbosityArg != "" {
			invocationFlags = append(invocationFlags, verbosityArg)
		}

		err = bootstrap.DcpRun(
			apiServerCtx,
			rootDir,
			kconfig,
			allExtensions,
			invocationFlags,
			log,
			runEvtHandlers,
		)

		return err
	}
}
