package dcpclient

import (
	"context"
	"fmt"
	"os"
	"time"

	clientgorest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/kubeconfig"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
)

const (
	DefaultServerConnectTimeout = 15 * time.Second
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
	ApplyDcpOptions(config)

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
// The standard controller runtime client factory function does dynamic discovery at startup.
// If the API server is not already running, it will fail with error immediately.
// NewClientFromConfig() will re-try client creation, with exponential back-off, until the time out is reached.
func NewClientFromConfig(timeoutCtx context.Context, config *clientgorest.Config) (ctrl_client.Client, error) {
	scheme := NewScheme()

	client, err := resiliency.RetryGetExponential(timeoutCtx, func() (ctrl_client.Client, error) {
		return ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
	})
	if err != nil {
		return nil, fmt.Errorf("could not create the client for the API server: %w", err)
	}

	_, err = resiliency.RetryGetExponential(timeoutCtx, func() (bool, error) {
		var exeList apiv1.ExecutableList
		listErr := client.List(timeoutCtx, &exeList, &ctrl_client.ListOptions{Limit: 1})
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
func NewConfigFromKubeconfigFile(timeoutCtx context.Context, kubeconfigPath string) (*clientgorest.Config, error) {
	config, configErr := resiliency.RetryGetExponential(timeoutCtx, func() (*clientgorest.Config, error) {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	})
	if configErr != nil {
		return nil, configErr
	}
	ApplyDcpOptions(config)
	return config, nil
}

func ApplyDcpOptions(config *clientgorest.Config) {
	// Do not throttle the client (we only deal with a handful of clients at most)
	// unless the rate of requests becomes insane.
	config.QPS = 1000
	config.Burst = 2000

	token, _ := os.LookupEnv(kubeconfig.DCP_SECURE_TOKEN)
	if token != "" {
		// If a token was supplied, use it to authenticate to the API server
		config.BearerToken = token
	}
}
