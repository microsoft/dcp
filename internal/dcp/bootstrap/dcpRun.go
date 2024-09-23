package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	"github.com/microsoft/usvc-apiserver/internal/hosting"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
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
	kconfig *kubeconfig.Kubeconfig,
	allExtensions []DcpExtension,
	invocationFlags []string,
	log logr.Logger,
	evtHandlers DcpRunEventHandlers,
) error {

	controllers := slices.Select(allExtensions, func(ext DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.ControllerCapability)
	})

	apiServer := apiserver.NewApiServer(string(extensions.ApiServerCapability), kconfig, log)

	hostedServices := []hosting.Service{}
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

	apiServerShutdown, apiServerErr := apiServer.Run(hostCtx)
	if apiServerErr != nil {
		cancelHostCtx()
		return apiServerErr
	}

	shutdownErrors, lifecycleMsgs := host.RunAsync(hostCtx)
	shutdownHost := func() error {
		cancelHostCtx()
		var allErrors error

		shutdownErr := <-shutdownErrors
		if shutdownErr != nil {
			log.Error(shutdownErr, "one or more hosted services failed to shut down gracefully")
			allErrors = errors.Join(allErrors, shutdownErr)
		}

		allErrors = errors.Join(allErrors, apiServer.Dispose())
		return allErrors
	}

	var err error
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

			case <-apiServerShutdown:
				err = fmt.Errorf("API server shut down unexpectedly. Graceful shutdown is not possible.")
				log.Error(err, "Terminating...")
				return errors.Join(err, shutdownHost())

			case msg, isOpen := <-lifecycleMsgs:
				if !isOpen {
					lifecycleMsgs = nil
					continue
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
