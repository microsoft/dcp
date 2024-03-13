package integration_test

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func ensureVolumeCreated(t *testing.T, ctx context.Context, volume *apiv1.ContainerVolume) containers.InspectedVolume {
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(volume), func(updatedVol *apiv1.ContainerVolume) (bool, error) {
		return len(updatedVol.Finalizers) > 0, nil
	})

	var inspected []containers.InspectedVolume
	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		inspectedVolumes, err := orchestrator.InspectVolumes(ctx, []string{volume.Spec.Name})
		if err != nil {
			if !errors.Is(err, containers.ErrNotFound) {
				return false, err
			}

			return false, nil
		}

		inspected = inspectedVolumes
		return true, nil
	})

	require.NoError(t, err, "could not inspect the volume")
	require.Len(t, inspected, 1, "expected to find a single volume")

	return inspected[0]
}

func TestVolumeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "volume-creation"

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: testName,
		},
	}

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Log("Ensure that a corresponding Docker volume was created...")
	_ = ensureVolumeCreated(t, ctx, &vol)
}

func TestVolumeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "volume-deletion"

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: testName,
		},
	}

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Logf("Ensure that ContainerVolume '%s' has a corresponding Docker volume created...", vol.ObjectMeta.Name)
	_ = ensureVolumeCreated(t, ctx, &vol)

	t.Logf("Deleting ContainerVolume '%s'...", vol.ObjectMeta.Name)
	err = retryOnConflict(ctx, vol.NamespacedName(), func(ctx context.Context, currentVol *apiv1.ContainerVolume) error {
		return client.Delete(ctx, currentVol)
	})
	require.NoError(t, err, "ContainerVolume object could not be deleted")

	t.Logf("Ensure that ContainerVolume '%s' object really disappeared from the API server...", vol.ObjectMeta.Name)
	waitObjectDeleted[apiv1.ContainerVolume](t, ctx, ctrl_client.ObjectKeyFromObject(&vol))

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, inspectionErr := orchestrator.InspectVolumes(ctx, []string{testName})
		if inspectionErr != nil {
			if errors.Is(inspectionErr, containers.ErrNotFound) {
				return true, nil
			}

			return false, inspectionErr
		}

		return false, nil
	})
	require.NoError(t, err, "Could not ensure that the volume was deleted")
}
