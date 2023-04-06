package appmgmt

import (
	"context"
	"errors"
	"fmt"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_config "sigs.k8s.io/controller-runtime/pkg/client/config"

	apiv1 "github.com/usvc-dev/stdtypes/api/v1"
)

const (
	deletionTimeoutSeconds = 15
)

func ShutdownApp(ctx context.Context) error {
	client, err := getClient()
	if err != nil {
		return err
	}

	// Delete Executables and Containers first, then ContainerVolumes, if any
	kinds := []ctrl_client.Object{&apiv1.Executable{}, &apiv1.Container{}, &apiv1.ContainerVolume{}}
	deletionErrors := []error{}
	for _, kind := range kinds {
		err := client.DeleteAllOf(ctx, kind, ctrl_client.GracePeriodSeconds(deletionTimeoutSeconds))
		if err != nil {
			deletionErrors = append(deletionErrors, fmt.Errorf("Could not delete all objects of type '%s': %w", kind.GetObjectKind().GroupVersionKind().Kind, err))
		}
	}

	if len(deletionErrors) > 0 {
		return errors.Join(deletionErrors...)
	}
	return nil
}

func getClient() (ctrl_client.Client, error) {
	config, err := ctrl_config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("Could not configure the client for the API server: %w", err)
	}

	scheme := apiruntime.NewScheme()
	if err = apiv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("Could not add standard type information to the client: %w", err)
	}

	client, err := ctrl_client.New(config, ctrl_client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("Could not create the client for the API server: %w", err)
	}
	return client, nil
}
