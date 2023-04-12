package appmgmt

import (
	"context"
	"errors"
	"fmt"
	"time"

	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usvc-dev/apiserver/pkg/dcpclient"
	apiv1 "github.com/usvc-dev/stdtypes/api/v1"
)

const (
	deletionTimeoutSeconds = 15
)

func ShutdownApp(ctx context.Context) error {
	client, err := dcpclient.New(ctx, 5*time.Second)
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
