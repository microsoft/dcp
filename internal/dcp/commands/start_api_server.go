/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/dcp/bootstrap"
	"github.com/microsoft/dcp/internal/perftrace"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/kubeconfig"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

var (
	rootDir    string
	detach     bool
	serverOnly bool
)

func NewStartApiSrvCommand(log logr.Logger) (*cobra.Command, error) {
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
	container_flags.EnsureTestContainerOrchestratorSocketFlag(startApiSrvCmd.Flags())
	cmds.AddMonitorFlags(startApiSrvCmd)

	return startApiSrvCmd, nil
}

func startApiSrv(log logr.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		log = log.WithName("start-apiserver")

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

			if !hasContainerRuntimeFlag && container_flags.GetRuntimeFlagValue() != container_flags.UnknownRuntime {
				args = append(args, container_flags.GetRuntimeFlag(), string(container_flags.GetRuntimeFlagValue()))
			}

			log.V(1).Info("Forking command", "Cmd", os.Args[0], "Args", args)

			usvc_io.PreserveSessionFolder() // The forked process will take care of cleaning up the session folder

			// We assume os.Args[0] will be valid here because it is in all the scenarios we currently support.
			forked := exec.Command(os.Args[0], args...)
			forked.Env = os.Environ()    // Inherit the environment from the parent process
			logger.WithSessionId(forked) // Ensure the session ID is passed to the forked process
			process.ForkFromParent(forked)

			if err := forked.Start(); err != nil {
				log.Error(err, "Forked process failed to run")
				return err
			} else {
				log.V(1).Info("Forked process started", "PID", forked.Process.Pid)
			}

			if err := forked.Process.Release(); err != nil {
				log.Error(err, "Release failed for process", "PID", forked.Process.Pid)
				return err
			}

			return nil
		}

		err := perftrace.CaptureStartupProfileIfRequested(apiServerCtx, log)
		if err != nil {
			log.Error(err, "Failed to capture startup profile")
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

		invocationFlags := []string{"--kubeconfig", kconfig.Path()}
		if container_flags.GetRuntimeFlagValue() != container_flags.UnknownRuntime {
			invocationFlags = append(invocationFlags, container_flags.GetRuntimeFlag(), string(container_flags.GetRuntimeFlagValue()))
		}
		if verbosityArg := logger.GetVerbosityArg(cmd.Flags()); verbosityArg != "" {
			invocationFlags = append(invocationFlags, verbosityArg)
		}

		err = bootstrap.DcpRun(
			apiServerCtx,
			rootDir,
			kconfig,
			serverOnly,
			allExtensions,
			invocationFlags,
			log,
		)

		return err
	}
}
