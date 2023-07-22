package appmgmt

import (
	"context"
	"errors"
	"fmt"
	"time"

	wait "k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/microsoft/usvc-apiserver/pkg/dcpclient"
	apiv1 "github.com/microsoft/usvc-stdtypes/api/v1"
	"github.com/microsoft/usvc-stdtypes/pkg/commonapi"
)

func ShutdownApp(ctx context.Context) error {
	client, err := dcpclient.New(ctx, 5*time.Second)
	if err != nil {
		return err
	}

	// Delete Executables and Containers first, then ContainerVolumes, if any.
	// Do not use client.DeleteAllOf() because it does not seem to give the controllers an opportunity to handle the deletion
	// (probably a bug in controller-runtime or Tilt API server). E.g. https://github.com/kubernetes-sigs/controller-runtime/issues/1842
	kinds := []commonapi.ListWithObjectItems{&apiv1.ExecutableReplicaSetList{}, &apiv1.ExecutableList{}, &apiv1.ContainerList{}, &apiv1.ContainerVolumeList{}}
	shutdownErrors := []error{}

	for _, objList := range kinds {
		err := client.List(ctx, objList)
		if err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("could not list '%s': %w", objList.GetObjectKind().GroupVersionKind().Kind, err))
			continue
		}

		items := objList.GetItems()
		for _, item := range items {
			err := client.Delete(ctx, item)
			if err != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("could not delete '%s': %w", item.GetObjectKind().GroupVersionKind().Kind, err))
			}
		}
	}

	if len(shutdownErrors) > 0 {
		return fmt.Errorf("not all application assets could be deleted: %w", errors.Join(shutdownErrors...))
	}

	err = waitAllDeleted(ctx, client, kinds)
	if err != nil {
		return err
	}

	return nil
}

func waitAllDeleted(ctx context.Context, client client.Client, kinds []commonapi.ListWithObjectItems) error {
	allDeleted := func(ctx context.Context) (bool, error) {
		for _, objList := range kinds {
			listErr := client.List(ctx, objList)
			if listErr != nil {
				return false, listErr
			}
			if objList.ItemCount() > 0 {
				return false, nil
			}
		}
		return true, nil
	}

	const pollImmediately = true // Don't wait before polling for the first time
	err := wait.PollUntilContextCancel(ctx, 1*time.Second, pollImmediately, allDeleted)
	if err != nil {
		return fmt.Errorf("could not ensure that all application assets were deleted: %w", err)
	} else {
		return nil
	}
}
