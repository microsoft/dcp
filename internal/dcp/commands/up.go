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

	"github.com/microsoft/usvc-apiserver/internal/appmgmt"
	"github.com/microsoft/usvc-apiserver/internal/dcp/bootstrap"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type upFlagData struct {
	appRootDir string
	renderer   string
}

var (
	upFlags upFlagData
)

func NewUpCommand(log logger.Logger) (*cobra.Command, error) {
	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Runs an application",
		Long: `Runs an application.

This command currently supports only Azure CLI-enabled applications of certain types.`,
		RunE: runApp(log),
		Args: cobra.NoArgs,
	}

	kubeconfig.EnsureKubeconfigFlag(upCmd.Flags())
	kubeconfig.EnsureKubeconfigPortFlag(upCmd.Flags())

	upCmd.Flags().StringVarP(&upFlags.appRootDir, "root-dir", "r", "", "If present, tells DCP to use specific directory as the application root directory. Defaults to current working directory.")
	upCmd.Flags().StringVarP(&upFlags.renderer, "app-type", "", "", "Specifies the type of application to run, if the type cannot be inferred unambiguously.")

	return upCmd, nil
}

func runApp(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("up")

		appRootDir := upFlags.appRootDir
		var err error
		if appRootDir == "" {
			appRootDir, err = os.Getwd()
			if err != nil {
				log.Error(err, "could not determine the working directory")
				return fmt.Errorf("could not determine the working directory: %w", err)
			}
		}

		kubeconfigPath, err := kubeconfig.EnsureKubeconfigFlagValue(cmd.Flags())
		if err != nil {
			return err
		}

		commandCtx, cancelCommandCtx := context.WithCancel(kubeapiserver.SetupSignalContext())
		defer cancelCommandCtx()

		err = perftrace.CaptureStartupProfileIfRequested(commandCtx, log)
		if err != nil {
			log.Error(err, "failed to capture startup profile")
		}

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
				renderErr := effRenderer.Render(commandCtx, appRootDir, kubeconfigPath)
				return renderErr
			},
			BeforeApiSrvShutdown: func() error {
				// Shut down the application.
				//
				// Don't use commandCtx here--it is already cancelled when this function is called,
				// so using it would result in immediate failure.
				shutdownCtx, cancelShutdownCtx := context.WithTimeout(context.Background(), 1*time.Minute)
				defer cancelShutdownCtx()
				log.Info("Stopping the application...")
				shutdownErr := appmgmt.ShutdownApp(shutdownCtx, log)
				if shutdownErr != nil {
					return fmt.Errorf("could not shut down the application gracefully: %w", shutdownErr)
				} else {
					log.Info("Application stopped.")
					return nil
				}
			},
		}

		invocationFlags := []string{"--kubeconfig", kubeconfigPath}
		if verbosityArg := logger.GetVerbosityArg(cmd.Flags()); verbosityArg != "" {
			invocationFlags = append(invocationFlags, verbosityArg)
		}
		err = bootstrap.DcpRun(commandCtx, appRootDir, log, allExtensions, invocationFlags, runEvtHandlers)
		return err
	}
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
		return bootstrap.DcpExtension{}, fmt.Errorf("no application runners found. Check DCP installation")
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
			return bootstrap.DcpExtension{}, fmt.Errorf("the specified application type '%s' does not match application located in '%s': %s", candidate.Id, appRootDir, canRenderResponse.Reason)
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
			eList = append(eList, fmt.Errorf("could not determine whether application type '%s' can be started: %w", renderers[i].Id, r.Err))
		} else {
			retval[i] = r.Response
		}
	}

	return retval, errors.Join(eList...)
}
