package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCmd(logger logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcpd",
		Short:         "Runs the DCP API server",
		Long: `DCP is a developer tool for running multi-service applications.

	This executable is the DCP API server, which holds the application workload model(s),
	and facilitates communication between DCP CLI, application renderers, and DCP controllers.

	By default (no command specified), this executable runs the DCP API server.
	`,
		RunE: runApiServer(logger),
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(logger); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(NewGetCapabilitiesCommand())

	kubeconfig.EnsureKubeconfigFlag(rootCmd.Flags())
	kubeconfig.EnsureKubeconfigPortFlag(rootCmd.Flags())

	cmds.AddMonitorFlags(rootCmd)

	container_flags.EnsureRuntimeFlag(rootCmd.PersistentFlags())
	logger.AddLevelFlag(rootCmd.PersistentFlags())

	klog.SetLogger(logger.V(1))
	ctrlruntime.SetLogger(logger.V(1))

	return rootCmd, nil
}

func runApiServer(log logger.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		err := perftrace.CaptureStartupProfileIfRequested(cmd.Context(), log.Logger)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

		ctx := cmds.Monitor(cmd.Context(), log.WithName("monitor"))

		apiServer := apiserver.NewApiServer(string(extensions.ApiServerCapability), log)

		err = apiServer.Run(ctx)
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		} else {
			return err
		}
	}
}
