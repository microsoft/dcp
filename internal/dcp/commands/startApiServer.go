package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/appmgmt"
	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcp/bootstrap"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/pkg/containers"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var (
	rootDir string
	detach  bool
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
	containers.EnsureRuntimeFlag(startApiSrvCmd.Flags())

	startApiSrvCmd.Flags().StringVarP(&rootDir, "root-dir", "r", "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")
	startApiSrvCmd.Flags().BoolVar(&detach, "detach", false, "If present, instructs DCP to fork itself as a detached process.")

	cmds.AddMonitorFlags(startApiSrvCmd)

	return startApiSrvCmd, nil
}

func startApiSrv(log logger.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		log := log.WithName("start-apiserver")

		ctx := cmds.Monitor(cmd.Context(), log.WithName("monitor"))

		if detach {
			args := make([]string, 0, len(os.Args)-2)

			for _, arg := range os.Args[1:] {
				if arg != "--detach" {
					args = append(args, arg)
				}
			}

			log.V(1).Info("Forking command", "cmd", os.Args[0], "args", args)

			logger.PreserveSessionFolder() // The forked process will take care of cleaning up the session folder

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

		err := perftrace.CaptureStartupProfileIfRequested(ctx, log)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

		if rootDir == "" {
			rootDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("could not determine the working directory: %w", err)
			}
		}

		kubeconfigPath, err := kubeconfig.EnsureKubeconfigFlagValue(cmd.Flags())
		if err != nil {
			return err
		}

		allExtensions, err := bootstrap.GetExtensions(ctx)
		if err != nil {
			return err
		}

		runEvtHandlers := bootstrap.DcpRunEventHandlers{
			BeforeApiSrvShutdown: func() error {
				// Shut down the application.
				//
				// Don't use ctx here--it is already cancelled when this function is called,
				// so using it would result in immediate failure.
				shutdownCtx, cancelShutdownCtx := context.WithTimeout(context.Background(), 1*time.Minute)
				defer cancelShutdownCtx()
				log.Info("Stopping the application...")
				shutdownErr := appmgmt.ShutdownApp(shutdownCtx, log.WithName("shutdown").V(1))
				if shutdownErr != nil {
					log.Error(shutdownErr, "could not shut down the application gracefully")
					return fmt.Errorf("could not shut down the application gracefully: %w", shutdownErr)
				} else {
					log.Info("Application stopped.")
					return nil
				}
			},
		}

		invocationFlags := []string{"--kubeconfig", kubeconfigPath, "--monitor", strconv.Itoa(os.Getpid()), containers.GetRuntimeFlag(), containers.GetRuntimeFlagArg()}
		if verbosityArg := logger.GetVerbosityArg(cmd.Flags()); verbosityArg != "" {
			invocationFlags = append(invocationFlags, verbosityArg)
		}
		err = bootstrap.DcpRun(ctx, rootDir, log, allExtensions, invocationFlags, runEvtHandlers)

		return err
	}
}
