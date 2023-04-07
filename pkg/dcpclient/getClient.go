package dcpclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/usvc-dev/stdtypes/api/v1"
)

// New() returns new client for the DCP API server.
//
// The standard controller runtime client.New() function does dynamic discovery at startup.
// If the API server is not already running, it will fail with error immediately.
// Our function will re-try client creation, with exponential back-off, until the time out is reached.
func New(ctx context.Context, timeout time.Duration) (ctrl_client.Client, error) {
	config, err := ctrl_config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("Could not configure the client for the API server: %w", err)
	}

	scheme := apiruntime.NewScheme()
	if err = apiv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("Could not add standard type information to the client: %w", err)
	}

	retryCtx, cancelRetryCtx := context.WithTimeout(ctx, timeout)
	defer cancelRetryCtx()
	var lastAttemptErr error

	client, err := backoff.RetryNotifyWithData(
		func() (ctrl_client.Client, error) {
			return ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
		},
		backoff.WithContext(backoff.NewExponentialBackOff(), retryCtx),
		func(err error, d time.Duration) {
			lastAttemptErr = err
		},
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("Could not create the client for the API server: timed out waiting for the API server to become available. Last error was: %w", lastAttemptErr)
		} else {
			return nil, fmt.Errorf("Could not create the client for the API server: %w", err)
		}
	}

	return client, nil
}
