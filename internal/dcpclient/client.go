package dcpclient

import (
	"context"
	"fmt"
	"time"

	clientgorest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
)

// NewClient() returns new client for the DCP API server.
//
// The standard controller runtime client factory function does dynamic discovery at startup.
// If the API server is not already running, it will fail with error immediately.
// NewClient() will re-try client creation, with exponential back-off, until the time out is reached.
func NewClient(ctx context.Context, timeout time.Duration) (ctrl_client.Client, error) {
	timeoutCtx, cancelTimeoutCtx := context.WithTimeout(ctx, timeout)
	defer cancelTimeoutCtx()

	config, err := ctrl_config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("could not configure the client for the API server: %w", err)
	}
	applyDcpOptions(config)

	scheme := NewScheme()

	client, err := resiliency.RetryGetExponential(timeoutCtx, func() (ctrl_client.Client, error) {
		return ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
	})
	if err != nil {
		return nil, fmt.Errorf("could not create the client for the API server: %w", err)
	}

	_, err = resiliency.RetryGetExponential(timeoutCtx, func() (bool, error) {
		var exeList apiv1.ExecutableList
		listErr := client.List(ctx, &exeList, &ctrl_client.ListOptions{Limit: 1})
		if listErr != nil {
			return false, listErr
		}

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not confirm API server listening: %w", err)
	}

	return client, nil
}

// NewClientFromConfig() returns new client for the DCP API server, using the provided configuration object.
//
// The standard controller runtime client factory function does dynamic discovery at startup.
// If the API server is not already running, it will fail with error immediately.
// NewClientFromConfig() will re-try client creation, with exponential back-off, until the time out is reached.
func NewClientFromKubeconfigFile(ctx context.Context, timeout time.Duration, config *clientgorest.Config) (ctrl_client.Client, error) {
	timeoutCtx, cancelTimeoutCtx := context.WithTimeout(ctx, timeout)
	defer cancelTimeoutCtx()

	scheme := NewScheme()

	client, err := resiliency.RetryGetExponential(timeoutCtx, func() (ctrl_client.Client, error) {
		return ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
	})
	if err != nil {
		return nil, fmt.Errorf("could not create the client for the API server: %w", err)
	}

	_, err = resiliency.RetryGetExponential(timeoutCtx, func() (bool, error) {
		var exeList apiv1.ExecutableList
		listErr := client.List(ctx, &exeList, &ctrl_client.ListOptions{Limit: 1})
		if listErr != nil {
			return false, listErr
		}

		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not confirm API server listening: %w", err)
	}

	return client, nil
}

// Creates a new Kubernetes client configuration from a kubeconfig file.
func NewConfigFromKubeconfigFile(kubeconfigPath string) (*clientgorest.Config, error) {
	config, configErr := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if configErr != nil {
		return nil, configErr
	}
	applyDcpOptions(config)
	return config, nil
}

func applyDcpOptions(config *clientgorest.Config) {
	// Do not throttle the client (we only deal with a handful of clients at most)
	// unless the rate of requests becomes insane.
	config.QPS = 1000
	config.Burst = 2000
}
