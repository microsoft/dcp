package commands

import (
	"flag"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/multierr"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/usvc-dev/stdtypes/api/v1"
	rnd "github.com/usvc-dev/stdtypes/renderers"
)

type upFlagData struct {
	appRootDir string
}

var (
	upFlags upFlagData
)

const (
	// Flag names
	appRootDirFlag      = "root-dir"
	appRootDirFlagShort = "r"
)

func NewUpCommand() *cobra.Command {
	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Runs an application",
		Long: `Runs an application.

This command currently supports only Azure CLI-enabled applications of certain types.`,
		RunE: runApp,
		Args: cobra.NoArgs,
	}

	// controller-runtime will register --kubeconfig flag in the default flag.CommandLine flag set.
	// See https://github.com/kubernetes-sigs/controller-runtime/blob/master/pkg/client/config/config.go for details.
	// The following line is necessary to ensure that this flag is recognized.
	upCmd.Flags().AddGoFlagSet(flag.CommandLine)

	upCmd.Flags().StringVarP(&upFlags.appRootDir, appRootDirFlag, appRootDirFlagShort, "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")

	return upCmd
}

var (
	renderers = []rnd.WorkloadRenderer{
		rnd.NewAzdRenderer(),
	}
)

func runApp(cmd *cobra.Command, args []string) error {
	appRootDir := upFlags.appRootDir
	var err error
	if appRootDir == "" {
		appRootDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("Could not determine the working directory: %w", err)
		}
	}

	renderer, err := getRenderer(appRootDir)
	if err != nil {
		return err
	}

	// TODO: instead of assuming that DCPD is already running, we should launch it on demand.

	client, err := getClient()
	if err != nil {
		return err
	}

	workload, err := renderer.Render(cmd.Context(), appRootDir, client)
	if err != nil {
		return fmt.Errorf("Could not determine how to run the application: %w", err)
	}

	for _, obj := range workload {
		err = client.Create(cmd.Context(), obj, &ctrl_client.CreateOptions{})
		if err != nil {
			// TODO: "roll back", i.e. delete, all objects that have been created up to this point

			return fmt.Errorf("Application run failed. An error occurred when creating object '%s' of type '%s': %w", obj.GetName(), obj.GetObjectKind().GroupVersionKind().Kind, err)
		}
	}

	fmt.Fprintln(os.Stderr, "Application create successfully")
	return nil
}

func getRenderer(cwd string) (rnd.WorkloadRenderer, error) {
	// TODO: if more than one workload renderer is ready to render the application,
	// ask the user which one to use.
	// We should also have an invocation flag that tells DCP which renderer to use.

	var rendererErrs error = nil

	for _, r := range renderers {
		err := r.CanRender(cwd)
		if err == nil {
			return r, nil // We found one that will render the application
		} else {
			rendererErrs = multierr.Append(rendererErrs, err)
		}
	}

	return nil, multierr.Combine(fmt.Errorf("The application cannot be started"), rendererErrs)
}

func getClient() (ctrl_client.Client, error) {
	config, err := ctrl_config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("Could not configure the client for the API server: %w", err)
	}

	scheme := apiruntime.NewScheme()
	if err = apiv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("Could not add standard type information to the client: %w", err)
	}

	client, err := ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("Could not create the client for the API server: %w", err)
	}
	return client, nil
}
