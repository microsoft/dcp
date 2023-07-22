package dcpclient

import (
	"context"
	"fmt"
	"time"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	apiv1 "github.com/microsoft/usvc-stdtypes/api/v1"
)

// New() returns new client for the DCP API server.
//
// The standard controller runtime client.New() function does dynamic discovery at startup.
// If the API server is not already running, it will fail with error immediately.
// Our function will re-try client creation, with exponential back-off, until the time out is reached.
func New(ctx context.Context, timeout time.Duration) (ctrl_client.Client, error) {
	timeoutCtx, cancelTimeoutCtx := context.WithTimeout(ctx, timeout)
	defer cancelTimeoutCtx()

	config, err := ctrl_config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("could not configure the client for the API server: %w", err)
	}

	scheme := apiruntime.NewScheme()
	if err = apiv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("could not add standard type information to the client: %w", err)
	}

	client, err := resiliency.RetryGet(timeoutCtx, func() (ctrl_client.Client, error) {
		return ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
	})
	if err != nil {
		return nil, fmt.Errorf("could not create the client for the API server: %w", err)
	}

	if _, err := resiliency.RetryGet(timeoutCtx, func() (bool, error) {
		var exeList apiv1.ExecutableList
		if err := client.List(ctx, &exeList, &ctrl_client.ListOptions{Limit: 1}); err != nil {
			return false, err
		}

		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("could not confirm API server listening: %w", err)
	}

	return client, nil
}
