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
		return fmt.Errorf("no API servers found")
	} else if len(apiServerExtensions) > 1 {
		return fmt.Errorf("multiple API servers found")
	}
	apiServerSvc, err := NewDcpExtensionService(cwd, apiServerExtensions[0], "", invocationFlags, log)
	if err != nil {
		return fmt.Errorf("could not start the API server: %w", err)
	}

	hostedServices := []hosting.Service{apiServerSvc}
	for _, controller := range controllers {
		controllerService, ctrlCreationErr := NewDcpExtensionService(cwd, controller, "run-controllers", invocationFlags, log)
		if ctrlCreationErr != nil {
			return fmt.Errorf("could not start controller '%s': %w", controller.Name, ctrlCreationErr)
		}
		hostedServices = append(hostedServices, controllerService)
	}

	hostCtx, cancelHostCtx := context.WithCancel(context.Background())
	host := &hosting.Host{
		Services: hostedServices,
		Logger:   log.WithName("dcp-host"),
	}
	shutdownErrors, lifecycleMsgs := host.RunAsync(hostCtx)
	shutdownHost := func() error {
		cancelHostCtx()
		shutdownErr := <-shutdownErrors
		if shutdownErr != nil {
			log.Error(shutdownErr, "one or more services failed to shut down gracefully")
			return fmt.Errorf("the API server or some controllers failed to shut down gracefully: %w", shutdownErr)
		} else {
			return nil
		}
	}

	if evtHandlers.AfterApiSrvStart != nil {
		if err = evtHandlers.AfterApiSrvStart(); err != nil {
			return errors.Join(err, shutdownHost())
		}
	}

	// Wait for the user to signal that they want to shut down.
	err = func() error {
		for {
			select {
			case <-ctx.Done():
				// We are being asked to shut down.
				log.Info("Shutting down...")
				return nil

			case msg := <-lifecycleMsgs:
				if msg.Err != nil && msg.ServiceName == apiServerSvc.Name() {
					log.Error(msg.Err, "API server exited with an error. Graceful shutdown is not possible. Terminating...")

					err = fmt.Errorf("API server exited with an error: %w.\nGraceful shutdown is not possible. Terminating...", msg.Err)
					return errors.Join(err, shutdownHost())
				}

				if msg.Err != nil {
					log.Error(msg.Err, fmt.Sprintf("Controller '%s' exited with an error. Application may not function correctly.", msg.ServiceName))
					// Let the user decide whether to continue or not, do not break the loop yet.
				}
			}
		}
	}()

	if err != nil {
		return err
	}

	if evtHandlers.BeforeApiSrvShutdown != nil {
		log.V(1).Info("Invoking BeforeApiSrvShutdown event handler.")
		if err = evtHandlers.BeforeApiSrvShutdown(); err != nil {
			log.Error(err, "BeforeApiSrvShutdown event handler failed.")
			return errors.Join(err, shutdownHost())
		}
	}

	if err = shutdownHost(); err != nil {
		return err
	} else {
		log.Info("Shutdown complete.")
		return nil
	}
}
