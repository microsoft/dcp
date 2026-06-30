/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/dcp/bootstrap"
	"github.com/microsoft/dcp/internal/perftrace"
	"github.com/microsoft/dcp/internal/telemetry"
	"github.com/microsoft/dcp/internal/telemetry/startupspans"
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
	kubeconfig.EnsureTLSCertFileFlag(startApiSrvCmd.Flags())
	kubeconfig.EnsureTLSKeyFileFlag(startApiSrvCmd.Flags())
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
	return func(cmd *cobra.Command, _ []string) error {
		log = log.WithName("start-apiserver")

		apiServerCtx, apiServerCtxCancel := cmds.GetMonitorContextFromFlags(cmd.Context(), log.WithName("monitor"))
		defer apiServerCtxCancel()

		if detach {
			return runDetachedFork(apiServerCtx, log)
		}

		// At this point we are the actual API-server-hosting process. Open the
		// bounded "dcp.startup" span that ends when the API server listener is up
		// — that is the moment Aspire's `ensure_kubernetes_client` finishes
		// waiting on the kubeconfig and the listener. We deliberately end the span
		// before host services finish starting because Aspire only needs the API
		// server reachable to proceed.
		startupCtx, span := startupspans.BeginStartup(apiServerCtx, serverOnly)
		var endStartupOnce sync.Once
		endStartup := func() {
			endStartupOnce.Do(func() {
				span.End()
				telemetry.ForceFlushStartup(log)
			})
		}
		defer endStartup()

		err := perftrace.CaptureStartupProfileIfRequested(apiServerCtx, log)
		if err != nil {
			log.Error(err, "Failed to capture startup profile")
		}

		defer usvc_io.CleanupSessionFolderIfNeeded()

		if rootDir == "" {
			rootDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("could not determine the working directory: %w", err)
			}
		}

		kconfig, err := telemetry.CallWithTelemetry(telemetry.StartupTracer(), startupspans.SpanEnsureKubeconfig, startupCtx,
			func(ctx context.Context) (*kubeconfig.Kubeconfig, error) {
				return kubeconfig.EnsureKubeconfigData(ctx, cmd.Flags(), log)
			},
		)
		if err != nil {
			return err
		}

		var allExtensions []bootstrap.DcpExtension
		if !serverOnly {
			allExtensions, err = telemetry.CallWithTelemetry(telemetry.StartupTracer(), startupspans.SpanGetExtensions, startupCtx,
				func(ctx context.Context) ([]bootstrap.DcpExtension, error) {
					exts, extErr := bootstrap.GetExtensions(ctx, log)
					if extErr == nil {
						startupspans.SetExtensionCount(ctx, len(exts))
					}
					return exts, extErr
				},
			)
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

		return bootstrap.DcpRun(
			startupCtx,
			rootDir,
			kconfig,
			serverOnly,
			allExtensions,
			invocationFlags,
			endStartup,
			log,
		)
	}
}

// runDetachedFork handles the `--detach` path: re-exec the dcp binary in the background
// without the --detach flag. The parent process exits immediately. The "dcp.startup.parent_fork"
// span captures the small but real cost of the spawn step; parent and child end up as
// siblings under the same outer trace because they read the same DCP_OTEL_STARTUP_TRACEPARENT.
func runDetachedFork(apiServerCtx context.Context, log logr.Logger) error {
	_, span := startupspans.BeginParentFork(apiServerCtx)
	defer func() {
		span.End()
		telemetry.ForceFlushStartup(log)
	}()

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

	forked := exec.Command(os.Args[0], args...)
	forked.Env = os.Environ()    // Inherit the environment from the parent process
	logger.WithSessionId(forked) // Ensure the session ID is passed to the forked process
	process.ForkFromParent(forked)

	if err := forked.Start(); err != nil {
		log.Error(err, "Forked process failed to run")
		return err
	}
	log.V(1).Info("Forked process started", "PID", forked.Process.Pid)

	startupspans.SetChildPid(span, forked.Process.Pid)

	if err := forked.Process.Release(); err != nil {
		log.Error(err, "Release failed for process", "PID", forked.Process.Pid)
		return err
	}

	return nil
}
