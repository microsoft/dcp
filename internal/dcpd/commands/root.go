package commands

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/usvc-dev/apiserver/internal/apiserver"
	"github.com/usvc-dev/apiserver/pkg/extensions"
	"github.com/usvc-dev/apiserver/pkg/logger"
)

var (
	rootCmdLogger      logr.Logger
	rootCmdFlushLogger func()
)

func NewRootCmd() (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		Use:   "dcpd",
		Short: "Runs the DCP API server",
		Long: `DCP is a developer tool for running multi-service applications.

	This executable is the DCP API server, which holds the application workload model(s),
	and facilitates communication between DCP CLI, application renderers, and DCP controllers.`,
		RunE: runApiServer,
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			rootCmdFlushLogger()
		},
	}

	rootCmd.AddCommand(NewGetCapabilitiesCommand())

	rootCmdLogger, rootCmdFlushLogger = logger.NewLogger(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(rootCmdLogger)

	return rootCmd, nil
}

func runApiServer(cmd *cobra.Command, _ []string) error {
	apiServer := apiserver.NewApiServer(string(extensions.ApiServerCapability), rootCmdFlushLogger)
	err := apiServer.Run(cmd.Context())
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	} else {
		return err
	}
}
