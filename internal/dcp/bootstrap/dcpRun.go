package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/go-logr/logr"

	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	"github.com/microsoft/usvc-apiserver/internal/appmgmt"
	"github.com/microsoft/usvc-apiserver/internal/hosting"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type DcpRunEventHandlers struct {
	AfterApiSrvStart     func() error
	BeforeApiSrvShutdown func(apiserver.ApiServerResourceCleanup) error
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
) error {
	// If the context is already complete, we should not proceed with running the API server and controllers.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	controllers := slices.Select(allExtensions, func(ext DcpExtension) bool {
		return slices.Contains(ext.Capabilities, extensions.ControllerCapability)
	})

	apiServer := apiserver.NewApiServer(string(extensions.ApiServerCapability), kconfig, log)

	cleanupCtx, cancelCleanupCtx := context.WithCancel(ctx)
	defer cancelCleanupCtx()

	// This context is used to trigger shutdown of the API server.
	apiServerCtx, cancelApiServerCtx := context.WithCancel(context.Background())
	defer cancelApiServerCtx()

	// This context is used to trigger shutdown of the controllers (or other extensions).
	// We intentionally use context.Background() here to allow us to control the timing of when
	// we shutdown the controllers.
	hostCtx, cancelHostCtx := context.WithCancel(context.Background())
	defer cancelHostCtx()

	hostedServices := []hosting.Service{}
	for _, controller := range controllers {
		controllerService, ctrlCreationErr := NewDcpExtensionService(cwd, controller, "run-controllers", invocationFlags, log)
		if ctrlCreationErr != nil {
			return fmt.Errorf("could not start controller '%s': %w", controller.Name, ctrlCreationErr)
		}
		hostedServices = append(hostedServices, controllerService)
	}

	host := &hosting.Host{
		Services: hostedServices,
		Logger:   log.WithName("dcp-host"),
	}

	var requestedResourceCleanup atomic.Value
	requestedResourceCleanup.Store(apiserver.ApiServerResourceCleanupFull) // By default we do full cleanup on shutdown.

	apiServerShutdown, apiServerErr := apiServer.Run(apiServerCtx, func(cleanup apiserver.ApiServerResourceCleanup) {
		requestedResourceCleanup.Store(cleanup)
		cancelCleanupCtx()
	})
	if apiServerErr != nil {
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
	// Wait for the user to signal that they want to shut down.
	for {
		select {
		case <-cleanupCtx.Done():
			// We are being asked to shut down.
			log.Info("Shutting down...")

			// Determine what level of resource cleanup is requested.
			resourceCleanup := requestedResourceCleanup.Load().(apiserver.ApiServerResourceCleanup)
			log.V(1).Info("Invoking BeforeApiSrvShutdown event handler.", "ResourceCleanup", resourceCleanup)

			// If we are in server-only mode (no standard controllers) such as when running tests,
			// there is no point trying to clean up all resources on shutdown because no actual resources are involved,
			// it is all test mocks. Another case to avoid full cleanup is when shutdown request explicitly disables it.
			if len(allExtensions) == 0 || !resourceCleanup.IsFull() {
				return nil // No cleanup needed, just return
			}

			err = appmgmt.CleanupAllResources(log)
			err = errors.Join(err, shutdownHost())
			if err != nil {
				log.Error(err, "Failed to cleanup some resources. This may lead to resource leaks.")
				return err
			}

			log.Info("Shutdown complete.")
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
}
