package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	kubeapiserver "k8s.io/apiserver/pkg/server"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usvc-dev/apiserver/internal/dcp/bootstrap"
	"github.com/usvc-dev/apiserver/pkg/kubeconfig"
)

var rootDir string

func NewStartApiSrvCommand() (*cobra.Command, error) {
	startApiSrvCmd := &cobra.Command{
		Use:    "start-apiserver",
		Short:  "Starts the API server and controllers, but does not attempt to run any application",
		RunE:   startApiSrv,
		Args:   cobra.NoArgs,
		Hidden: true, // This command is mostly for testing
	}

	kubeconfig.EnsureKubeconfigFlag(startApiSrvCmd.Flags())

	startApiSrvCmd.Flags().StringVarP(&rootDir, "root-dir", "r", "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")

	return startApiSrvCmd, nil
}

func startApiSrv(cmd *cobra.Command, args []string) error {
	var err error
	if rootDir == "" {
		rootDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("Could not determine the working directory: %w", err)
		}
	}

	log := runtimelog.Log.WithName("start-apiserver")

	kubeconfigPath, err := kubeconfig.EnsureKubeconfigFile(cmd.Flags())
	if err != nil {
		return err
	}

	commandCtx, cancelCommandCtx := context.WithCancel(kubeapiserver.SetupSignalContext())
	defer cancelCommandCtx()

	allExtensions, err := bootstrap.GetExtensions(commandCtx)
	if err != nil {
		return err
	}

	err = bootstrap.DcpRun(commandCtx, rootDir, kubeconfigPath, log, allExtensions, bootstrap.DcpRunEventHandlers{})
	return err
}
