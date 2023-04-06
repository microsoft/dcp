package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kubeapiserver "k8s.io/apiserver/pkg/server"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usvc-dev/apiserver/internal/appmgmt"
	"github.com/usvc-dev/apiserver/internal/dcp/bootstrap"
	"github.com/usvc-dev/apiserver/pkg/extensions"
	"github.com/usvc-dev/apiserver/pkg/kubeconfig"
	"github.com/usvc-dev/stdtypes/pkg/maps"
	"github.com/usvc-dev/stdtypes/pkg/slices"
)

type upFlagData struct {
	appRootDir string
	renderer   string
}

var (
	upFlags upFlagData
)

func NewUpCommand() (*cobra.Command, error) {
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
	} else {
		return nil, fmt.Errorf("could not set up the --kubeconfig flag")
	}

	upCmd.Flags().StringVarP(&upFlags.appRootDir, "root-dir", "r", "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")

	upCmd.Flags().StringVarP(&upFlags.renderer, "app-type", "", "", "Specifies the type of application to run, if the type cannot be inferred unambiguously.")

	return upCmd, nil
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

	log := runtimelog.Log.WithName("up")

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

	effRenderer, err := getEffectiveRenderer(commandCtx, appRootDir, allExtensions)
	if err != nil {
		return err
	}

	runEvtHandlers := bootstrap.DcpRunEventHandlers{
		AfterApiSrvStart: func() error {
			// Start the application
			err := effRenderer.Render(commandCtx, appRootDir, kubeconfigPath)
			return err
		},
		BeforeApiSrvShutdown: func() error {
			// Shut down the application.
			shutdownCtx, cancelShutdownCtx := context.WithTimeout(commandCtx, 1*time.Minute)
			defer cancelShutdownCtx()
			err := appmgmt.ShutdownApp(shutdownCtx)
			if err != nil {
				return fmt.Errorf("Could not shut down the application gracefully: %w", err)
			} else {
				return nil
			}
		},
	}

	err = bootstrap.DcpRun(commandCtx, appRootDir, kubeconfigPath, log, allExtensions, runEvtHandlers)
	return err
}

// The logic of getEffectiveRenderer() is as follows:
// A. If there are no renderers available, just tell the user to reinstall DCP.
// B. If the user specified a renderer:
// B1. If the renderer is not available, error, giving the list of available renderers.
// B2. If the renderer is available, but cannot render, report error with the reason.
// B3. If the renderer is available and can render, use it.
//
// C. If the user did not specify a renderer:
// C1. If there are no renderers that can render, error, giving all the reasons why renderes cannot render.
// C2. If there is only one renderer that can render, use it.
// C3. If there are multiple renderers that can render, error, giving the list of renderers that can render.
func getEffectiveRenderer(ctx context.Context, appRootDir string, allExtensions []bootstrap.DcpExtension) (bootstrap.DcpExtension, error) {
	renderers := slices.Select(allExtensions, func(ext bootstrap.DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.WorkloadRendererCapability)
	})
	if len(renderers) == 0 {
		// A. No renderers available.
		return bootstrap.DcpExtension{}, fmt.Errorf("No application runners found. Check DCP installation.")
	}

	if upFlags.renderer != "" {
		// B. The user has specified a renderer.

		matching := slices.Select(renderers, func(r bootstrap.DcpExtension) bool { return r.Id == upFlags.renderer })
		if len(matching) != 1 {
			// B1: The specified renderer is not available.
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("The specified application type '%s' is not valid. Available application types are:\n", upFlags.renderer))
			for _, r := range renderers {
				sb.WriteString(fmt.Sprintf("%s (%s)\n", r.Id, r.Name))
			}
			return bootstrap.DcpExtension{}, fmt.Errorf(sb.String())
		}

		candidate := matching[0]

		canRenderResponse, err := candidate.CanRender(ctx, appRootDir)
		if err != nil {
			return bootstrap.DcpExtension{}, err // Unexpected error: the extension should be able to tell us whether it can render or not.
		}

		if canRenderResponse.Result == extensions.CanRenderResultNo {
			// B2: The specified renderer is available, but cannot render.
			return bootstrap.DcpExtension{}, fmt.Errorf("The specified application type '%s' does not match application located in '%s': %s", candidate.Id, appRootDir, canRenderResponse.Reason)
		} else {
			// B3: The specified renderer is available and can render.
			return candidate, nil
		}
	} else {
		// C. The user has not specified a renderer.
		// Need to gather responses to "can-render" from all available renderers.
		responses, err := whoCanRender(ctx, renderers, appRootDir)
		if err != nil {
			return bootstrap.DcpExtension{}, err // Unexpected error: all extension should be able to tell us whether it can render or not.
		}
		positiveResponses := maps.Select(responses, func(i int, resp extensions.CanRenderResponse) bool {
			return resp.Result == extensions.CanRenderResultYes
		})
		if len(positiveResponses) == 0 {
			// C1: No renderers can render.
			var sb strings.Builder
			sb.WriteString("No application runner can run the application. The reasouns are:\n")
			for i, resp := range responses {
				sb.WriteString(fmt.Sprintf("%s: %s\n", renderers[i].Name, resp.Reason))
			}
			return bootstrap.DcpExtension{}, fmt.Errorf(sb.String())
		} else if len(positiveResponses) == 1 {
			// C2: Only one renderer can render (success).
			index := maps.Keys(positiveResponses)[0]
			return renderers[index], nil
		} else {
			// C3: Multiple renderers can render.
			var sb strings.Builder
			sb.WriteString("You must specify an application runner to use. Applicable runners are: ")
			runners := maps.MapToSlice[int, extensions.CanRenderResponse, string](positiveResponses, func(i int, _ extensions.CanRenderResponse) string {
				return fmt.Sprintf("%s (%s)", renderers[i].Id, renderers[i].Name)
			})
			sb.WriteString(strings.Join(runners, ", "))
			sb.WriteString(".")
			return bootstrap.DcpExtension{}, fmt.Errorf(sb.String())
		}
	}
}

// Returns a map of renderer index to CanRenderResponse.
// The index refers to the passed-in renderers slice.
// Only renderers that gave valid response (i.e. no error occurred) are included in the map.
func whoCanRender(ctx context.Context, renderers []bootstrap.DcpExtension, appRootDir string) (map[int]extensions.CanRenderResponse, error) {
	const concurrency = uint16(4) // How many renderers to interrogate in parallel.
	type rendererResponseWithErr struct {
		Response extensions.CanRenderResponse
		Err      error
	}

	rendererResponses := slices.MapConcurrent[bootstrap.DcpExtension, rendererResponseWithErr](
		renderers,
		func(r bootstrap.DcpExtension) rendererResponseWithErr {
			resp, err := r.CanRender(ctx, appRootDir)
			return rendererResponseWithErr{resp, err}
		}, concurrency)

	retval := make(map[int]extensions.CanRenderResponse)
	var eList []error
	for i, r := range rendererResponses {
		if r.Err != nil {
			eList = append(eList, fmt.Errorf("Could not determine whether application type '%s' can be started: %w", renderers[i].Id, r.Err))
		} else {
			retval[i] = r.Response
		}
	}

	return retval, errors.Join(eList...)
}
