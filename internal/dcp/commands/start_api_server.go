/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/dcp/bootstrap"
	"github.com/microsoft/dcp/internal/perftrace"
	"github.com/microsoft/dcp/internal/telemetry"
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
	kubeconfig.EnsureTLSCertThumbprintFlag(startApiSrvCmd.Flags())
	kubeconfig.EnsureTLSCAFileFlag(startApiSrvCmd.Flags())

	startApiSrvCmd.Flags().StringVarP(&rootDir, "root-dir", "r", "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")
	startApiSrvCmd.Flags().BoolVar(&detach, "detach", false, "If present, instructs DCP to fork itself as a detached process.")
	startApiSrvCmd.Flags().BoolVar(&serverOnly, "server-only", false, "If present, instructs DCP to start only the API server and not the controllers. This is useful for testing the API server in isolation.")

	container_flags.EnsureRuntimeFlag(startApiSrvCmd.Flags())
	container_flags.EnsureTestContainerOrchestratorSocketFlag(startApiSrvCmd.Flags())
	cmds.AddMonitorFlags(startApiSrvCmd)

	return startApiSrvCmd, nil
}

func startApiSrv(log logr.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) (err error) {
		log = log.WithName("start-apiserver")
		cmdCtx, span := telemetry.StartStartupSpan(cmd.Context(), telemetry.StartupSpanStartApiServer)
		telemetry.SetAttribute(cmdCtx, telemetry.StartupAttributeCommandName, "start-apiserver")
		telemetry.SetAttribute(cmdCtx, telemetry.StartupAttributeDetach, detach)
		telemetry.SetAttribute(cmdCtx, telemetry.StartupAttributeServerOnly, serverOnly)
		spanEnded := false
		endStartupSpan := func(startErr error) {
			if !spanEnded {
				span.SetError(startErr)
				span.End()
				spanEnded = true
			}
		}
		defer func() {
			endStartupSpan(err)
		}()
		cmd.SetContext(cmdCtx)

		apiServerCtx, apiServerCtxCancel := cmds.GetMonitorContextFromFlags(cmdCtx, log.WithName("monitor"))
		defer apiServerCtxCancel()
		telemetry.AddEvent(cmdCtx, telemetry.StartupEventStartApiServerMonitorConfigured)

		if detach {
			forkCtx, forkSpan := telemetry.StartStartupSpan(cmdCtx, telemetry.StartupSpanStartApiServerFork)
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
			telemetry.SetAttribute(forkCtx, telemetry.StartupAttributeForkArgumentCount, len(args))
			telemetry.AddEvent(forkCtx, telemetry.StartupEventStartApiServerForkStarting)

			usvc_io.PreserveSessionFolder() // The forked process will take care of cleaning up the session folder

			forked := exec.Command(os.Args[0], args...)
			forked.Env = os.Environ()    // Inherit the environment from the parent process
			logger.WithSessionId(forked) // Ensure the session ID is passed to the forked process
			process.ForkFromParent(forked)

			if err := forked.Start(); err != nil {
				log.Error(err, "Forked process failed to run")
				forkSpan.SetError(err)
				forkSpan.End()
				return err
			} else {
				log.V(1).Info("Forked process started", "PID", forked.Process.Pid)
				telemetry.SetAttribute(forkCtx, telemetry.StartupAttributeForkPID, forked.Process.Pid)
				telemetry.AddEvent(forkCtx, telemetry.StartupEventStartApiServerForkStarted)
			}

			if err := forked.Process.Release(); err != nil {
				log.Error(err, "Release failed for process", "PID", forked.Process.Pid)
				forkSpan.SetError(err)
				forkSpan.End()
				return err
			}

			forkSpan.End()
			endStartupSpan(nil)
			return nil
		}

		err = perftrace.CaptureStartupProfileIfRequested(apiServerCtx, log)
		if err != nil {
			log.Error(err, "Failed to capture startup profile")
		}

		defer usvc_io.CleanupSessionFolderIfNeeded()

		if rootDir == "" {
			telemetry.AddEvent(cmdCtx, telemetry.StartupEventStartApiServerRootDirResolving)
			rootDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("could not determine the working directory: %w", err)
			}
		}
		telemetry.SetAttribute(cmdCtx, telemetry.StartupAttributeRootDirectoryBasename, filepath.Base(rootDir))

		kubeconfigCtx, kubeconfigSpan := telemetry.StartStartupSpan(cmdCtx, telemetry.StartupSpanStartApiServerEnsureKubeconfig)
		kconfig, err := kubeconfig.EnsureKubeconfigData(cmd.Flags(), log)
		if err == nil {
			telemetry.AddEvent(kubeconfigCtx, telemetry.StartupEventStartApiServerKubeconfigReady)
		}
		kubeconfigSpan.SetError(err)
		kubeconfigSpan.End()
		if err != nil {
			return err
		}

		var allExtensions []bootstrap.DcpExtension
		if !serverOnly {
			allExtensions, err = bootstrap.GetExtensions(cmdCtx, log)
			if err != nil {
				return err
			}
		}
		telemetry.SetAttribute(cmdCtx, telemetry.StartupAttributeExtensionsCount, len(allExtensions))

		invocationFlags := []string{"--kubeconfig", kconfig.Path()}
		if container_flags.GetRuntimeFlagValue() != container_flags.UnknownRuntime {
			invocationFlags = append(invocationFlags, container_flags.GetRuntimeFlag(), string(container_flags.GetRuntimeFlagValue()))
		}
		if verbosityArg := logger.GetVerbosityArg(cmd.Flags()); verbosityArg != "" {
			invocationFlags = append(invocationFlags, verbosityArg)
		}
		telemetry.SetAttribute(cmdCtx, telemetry.StartupAttributeInvocationFlagsCount, len(invocationFlags))

		endStartupSpan(nil)

		lifetimeCtx, lifetimeSpan := telemetry.StartStartupSpan(apiServerCtx, telemetry.StartupSpanStartApiServerLifetime)
		telemetry.SetAttribute(lifetimeCtx, telemetry.StartupAttributeServerOnly, serverOnly)
		telemetry.SetAttribute(lifetimeCtx, telemetry.StartupAttributeExtensionsCount, len(allExtensions))
		err = bootstrap.DcpRun(
			lifetimeCtx,
			rootDir,
			kconfig,
			serverOnly,
			allExtensions,
			invocationFlags,
			log,
		)
		lifetimeSpan.SetError(err)
		lifetimeSpan.End()

		return err
	}
}
