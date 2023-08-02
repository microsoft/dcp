package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/microsoft/usvc-apiserver/internal/hosting"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type DcpRunEventHandlers struct {
	AfterApiSrvStart     func() error
	BeforeApiSrvShutdown func() error
}

// DcpRun() starts the API server and controllers and waits for the signal to terminate them.
// It serves as a "skeleton" for commands such as "up" and "start-apiserver".
// Additional logic can be added into the API server lifecycle via evtHandlers parameter.
func DcpRun(
	ctx context.Context,
	cwd string,
	log logr.Logger,
	allExtensions []DcpExtension,
	invocationFlags []string,
	evtHandlers DcpRunEventHandlers,
) error {

	controllers := slices.Select(allExtensions, func(ext DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.ControllerCapability)
	})
	if len(controllers) == 0 {
		log.Info("No controllers found. Check DCP installation.")
	}

	// Start API server and controllers.
	apiServerExtensions := slices.Select(allExtensions, func(ext DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.ApiServerCapability)
	})
	if len(apiServerExtensions) == 0 {
		return fmt.Errorf("No API servers found. Check DCP installation.")
	} else if len(apiServerExtensions) > 1 {
		return fmt.Errorf("Multiple API servers found. Exactly one API server is required. Check DCP installation.")
	}
	apiServerSvc, err := NewDcpExtensionService(cwd, apiServerExtensions[0], "", invocationFlags)
	if err != nil {
		return fmt.Errorf("Could not start the API server: %w", err)
	}

	hostedServices := []hosting.Service{apiServerSvc}
	for _, controller := range controllers {
		controllerService, err := NewDcpExtensionService(cwd, controller, "run-controllers", invocationFlags)
		if err != nil {
			return fmt.Errorf("Could not start controller '%s': %w", controller.Name, err)
		}
		hostedServices = append(hostedServices, controllerService)
	}

	hostCtx, cancelHostCtx := context.WithCancel(context.Background())
	host := &hosting.Host{
		Services: hostedServices,
		Logger:   log,
	}
	shutdownErrors, lifecycleMsgs := host.RunAsync(hostCtx)
	shutdownHost := func() error {
		cancelHostCtx()
		shutdownErr := <-shutdownErrors
		if shutdownErr != nil {
			return fmt.Errorf("The API server or some controllers failed to shut down gracefully: %w", shutdownErr)
		} else {
			return nil
		}
	}

	if evtHandlers.AfterApiSrvStart != nil {
		if err := evtHandlers.AfterApiSrvStart(); err != nil {
			return errors.Join(err, shutdownHost())
		}
	}

	// Wait for the user to signal that they want to shut down.
serviceMessageLoop:
	for {
		select {
		case <-ctx.Done():
			// We are being asked to shut down.
			log.Info("Shutting down...")
			break serviceMessageLoop

		case msg := <-lifecycleMsgs:
			if msg.Err != nil && msg.ServiceName == apiServerSvc.Name() {
				err = fmt.Errorf("API server exited with an error: %w.\nGraceful shutdown is not possible. Terminating...", msg.Err)
				return errors.Join(err, shutdownHost())
			}

			if msg.Err != nil {
				log.Error(msg.Err, fmt.Sprintf("Controller '%s' exited with an error. Application may not function correctly.", msg.ServiceName))
				// Let the user decide whether to continue or not, do not break the loop yet.
			}
		}
	}

	if evtHandlers.BeforeApiSrvShutdown != nil {
		if err := evtHandlers.BeforeApiSrvShutdown(); err != nil {
			return errors.Join(err, shutdownHost())
		}
	}

	if err := shutdownHost(); err != nil {
		return err
	} else {
		log.Info("Shutdown complete.")
		return nil
	}
}
