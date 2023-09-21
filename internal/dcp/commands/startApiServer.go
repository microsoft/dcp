package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	kubeapiserver "k8s.io/apiserver/pkg/server"

	"github.com/microsoft/usvc-apiserver/internal/appmgmt"
	"github.com/microsoft/usvc-apiserver/internal/dcp/bootstrap"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

var rootDir string
var monitorPidInt64 int64 // Can't use process.Pid_t because the pflag package doesn't support nonstandard types
var monitorInterval uint8
var detach bool

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
	startApiSrvCmd.Flags().Int64VarP(&monitorPidInt64, "monitor", "m", int64(process.UnknownPID), "If present, tells DCP to monitor a given process ID (PID) and gracefully shutdown if the monitored process exits for any reason.")
	startApiSrvCmd.Flags().Uint8VarP(&monitorInterval, "monitor-interval", "i", 0, "If present, specifies the time in seconds between checks for the monitor PID.")
	startApiSrvCmd.Flags().BoolVar(&detach, "detach", false, "If present, instructs DCP to fork itself as a detached process.")

	return startApiSrvCmd, nil
}

func startApiSrv(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("start-apiserver")

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

		var err error
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

		commandCtx, cancelCommandCtx := context.WithCancel(kubeapiserver.SetupSignalContext())
		defer cancelCommandCtx()

		// If a monitor PID is set, find the process and ensure we shutdown DCP if it exits
		if monitorPidInt64 != int64(process.UnknownPID) {
			monitorPid, err := process.Int64ToPidT(monitorPidInt64)
			if err != nil {
				return err
			}

			monitorProc, err := process.FindWaitableProcess(monitorPid)
			if err != nil {
				return err
			}

			if monitorInterval > 0 {
				monitorProc.WaitPollInterval = time.Second * time.Duration(monitorInterval)
			}

			go func() {
				defer cancelCommandCtx()
				if err := monitorProc.Wait(commandCtx); err != nil {
					log.Error(err, "error waiting for process", "pid", monitorPid)
				} else {
					log.Info("monitor process exited, shutting down", "pid", monitorPid)
				}
			}()
		}

		allExtensions, err := bootstrap.GetExtensions(commandCtx)
		if err != nil {
			return err
		}

		runEvtHandlers := bootstrap.DcpRunEventHandlers{
			BeforeApiSrvShutdown: func() error {
				// Shut down the application.
				//
				// Don't use commandCtx here--it is already cancelled when this function is called,
				// so using it would result in immediate failure.
				shutdownCtx, cancelShutdownCtx := context.WithTimeout(context.Background(), 1*time.Minute)
				defer cancelShutdownCtx()
				log.Info("Stopping the application...")
				err := appmgmt.ShutdownApp(shutdownCtx, log)
				if err != nil {
					log.Error(err, "could not shut down the application gracefully")
					return fmt.Errorf("could not shut down the application gracefully: %w", err)
				} else {
					log.Info("Application stopped.")
					return nil
				}
			},
		}

		invocationFlags := []string{"--kubeconfig", kubeconfigPath}
		if verbosityArg := logger.GetVerbosityArg(cmd.Flags()); verbosityArg != "" {
			invocationFlags = append(invocationFlags, verbosityArg)
		}
		err = bootstrap.DcpRun(commandCtx, rootDir, log, allExtensions, invocationFlags, runEvtHandlers)

		return err
	}
}
