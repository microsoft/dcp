package commands

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCmd(logger logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		Use:   "dcpd",
		Short: "Runs the DCP API server",
		Long: `DCP is a developer tool for running multi-service applications.

	This executable is the DCP API server, which holds the application workload model(s),
	and facilitates communication between DCP CLI, application renderers, and DCP controllers.

	By default (no command specified), this executable runs the DCP API server.
	`,
		RunE: runApiServer(logger),
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			logger.Flush()
		},
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	rootCmd.AddCommand(NewGetCapabilitiesCommand())

	kubeconfig.EnsureKubeconfigFlag(rootCmd.Flags())
	kubeconfig.EnsureKubeconfigPortFlag(rootCmd.Flags())

	logger.AddLevelFlag(rootCmd.PersistentFlags())

	klog.SetLogger(logger.V(1))
	ctrlruntime.SetLogger(logger.V(1))

	return rootCmd, nil
}

func runApiServer(logger logger.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		apiServer := apiserver.NewApiServer(string(extensions.ApiServerCapability), logger)
		err := apiServer.Run(cmd.Context())
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		} else {
			return err
		}
	}
}
