package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	kubeapiserver "k8s.io/apiserver/pkg/server"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usvc-dev/apiserver/internal/dcp/bootstrap"
	"github.com/usvc-dev/apiserver/internal/dcp/extensions"
	"github.com/usvc-dev/apiserver/internal/hosting"
	"github.com/usvc-dev/apiserver/internal/kubeconfig"
	"github.com/usvc-dev/stdtypes/pkg/slices"
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

	// Make sure --kubeconfig flag is recognized
	if f := kubeconfig.GetKubeconfigFlag(nil); f != nil {
		upCmd.Flags().AddFlag(f)
	}

	upCmd.Flags().StringVarP(&upFlags.appRootDir, appRootDirFlag, appRootDirFlagShort, "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")

	return upCmd
}

func runApp(cmd *cobra.Command, args []string) error {
	appRootDir := upFlags.appRootDir
	var err error
	if appRootDir == "" {
		appRootDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("Could not determine the working directory: %w", err)
		}
	}

	ctx, cancelFn := context.WithCancel(kubeapiserver.SetupSignalContext())
	log := runtimelog.Log.WithName("up")

	// Discover extensions.
	allExtensions, err := extensions.GetExtensions(ctx)
	if err != nil {
		return err
	}
	controllers := slices.Select(allExtensions, func(ext extensions.DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.ControllerCapability)
	})
	rederers := slices.Select(allExtensions, func(ext extensions.DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.WorkloadRendererCapability)
	})

	// Start API server and controllers.
	kubeconfigPath, err := kubeconfig.EnsureKubeconfigFile(cmd.Flags())
	if err != nil {
		return err
	}

	apiServerSvc, err := bootstrap.NewDcpdService(kubeconfigPath)
	if err != nil {
		return err
	}
	hostedServices := []hosting.Service{}
	for _, controller := range controllers {
		// TODO: implement NewControllerService()
		controllerService, err := bootstrap.NewControllerService(controller)
	}

	host := &hosting.Host{
		Services: hostedServices,
		Logger:   log,
	}
	shutdownErrors, lifecycleMsgs := host.RunAsync(ctx)
	cancelFn()

	// Wait till user presses Ctrl-C or all controllers shut down (the latter is an error condition).
	// The following is NOT DONE YET.

	select {
	case <-ctx.Done():
		// The user pressed Ctrl-C
		log.Info("shutting down application...")
	case msg := <-lifecycleMsgs:
		// TODO: log error OR info that controller exited
		// TODO: special-case API server. API server failure should always terminate the whole command.
		log.Error(svcErr.Err, fmt.Sprintf("'%s' exited with an error", svcErr.Name))
	}

	// Finished shutting down. An error returned here is a failure to terminate gracefully,
	// so just crash if that happens.
	shutdownErr := <-shutdownErrors
	// TODO : handle shutdown error

	// TODO:
	// 1. Start API server
	// 2. Discover and start controllers.
	// 3. Discover and interrogate renderers.
	// 4. If more than one renderer reports ready, prompt the user to choose one.
	// The renderer will be responsible for creating the workload objects.

	// Old code, keep for reference
	/*

		renderer, err := getRenderer(appRootDir)
		if err != nil {
			return err
		}

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
	*/
}

/* TODO: old code, keep for reference
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
*/
