package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/apiserver"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func ensureVolumeCreated(
	t *testing.T,
	ctx context.Context,
	apiServerClient ctrl_client.Client,
	vo containers.VolumeOrchestrator,
	volume *apiv1.ContainerVolume,
) containers.InspectedVolume {
	waitObjectAssumesStateEx(t, ctx, apiServerClient, ctrl_client.ObjectKeyFromObject(volume), func(updatedVol *apiv1.ContainerVolume) (bool, error) {
		return updatedVol.Status.State == apiv1.ContainerVolumeStateReady, nil
	})

	var inspected []containers.InspectedVolume
	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		inspectedVolumes, err := vo.InspectVolumes(ctx, []string{volume.Spec.Name})
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

func TestContainerVolumeCreation(t *testing.T) {
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

	require.Nil(t, vol.Spec.Persistent, "Persistent flag can be omitted when creating a ContainerVolume")

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	require.True(t, pointers.TrueValue(vol.Spec.Persistent), "ContainerVolume should be persistent by default")

	t.Log("Ensure that a corresponding Docker volume was created...")
	_ = ensureVolumeCreated(t, ctx, client, containerOrchestrator, &vol)
}

// If persistent volume is deleted, the corresponding Docker volume should not be deleted.
func TestContainerVolumeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-volume-deletion"

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
	_ = ensureVolumeCreated(t, ctx, client, containerOrchestrator, &vol)

	t.Logf("Deleting ContainerVolume '%s'...", vol.ObjectMeta.Name)
	err = retryOnConflict(ctx, vol.NamespacedName(), func(ctx context.Context, currentVol *apiv1.ContainerVolume) error {
		return client.Delete(ctx, currentVol)
	})
	require.NoError(t, err, "ContainerVolume object could not be deleted")

	t.Logf("Ensure that ContainerVolume '%s' object really disappeared from the API server...", vol.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &vol)

	_, inspectedErr := containerOrchestrator.InspectVolumes(ctx, []string{testName})
	require.NoError(t, inspectedErr, "Could not ensure that the volume was not deleted")
}

// If nonpersistent volume is deleted, the corresponding Docker volume should be deleted as well.
func TestContainerVolumeDeletionNonpersistent(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "nonpersistent-volume-deletion"

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name:       testName,
			Persistent: new(bool), // false
		},
	}

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Logf("Ensure that ContainerVolume '%s' has a corresponding Docker volume created...", vol.ObjectMeta.Name)
	_ = ensureVolumeCreated(t, ctx, client, containerOrchestrator, &vol)

	t.Logf("Deleting ContainerVolume '%s'...", vol.ObjectMeta.Name)
	err = retryOnConflict(ctx, vol.NamespacedName(), func(ctx context.Context, currentVol *apiv1.ContainerVolume) error {
		return client.Delete(ctx, currentVol)
	})
	require.NoError(t, err, "ContainerVolume object could not be deleted")

	t.Logf("Ensure that ContainerVolume '%s' object really disappeared from the API server...", vol.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &vol)

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, inspectionErr := containerOrchestrator.InspectVolumes(ctx, []string{testName})
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

// Ensure that ContainerVolume objects are cleaned up when the API server is shutting down.
func TestContainerVolumeCleanup(t *testing.T) {
	t.Parallel()
	const testName = "container-volume-cleanup"

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	log := testutil.NewLogForTesting(t.Name())

	serverInfo, _, _, startupErr := StartTestEnvironment(ctx, VolumeController, t.Name(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	adminDocUrl := serverInfo.ClientConfig.Host + apiserver.AdminPathPrefix + apiserver.ExecutionDocument

	pVol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-persistent",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: testName + "-persistent",
		},
	}
	npVol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-nonpersistent",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name:       testName + "-nonpersistent",
			Persistent: new(bool), // false
		},
	}
	for _, vol := range []*apiv1.ContainerVolume{&pVol, &npVol} {
		t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
		err := serverInfo.Client.Create(ctx, vol)
		require.NoError(t, err, "Could not create a ContainerVolume object")

		t.Logf("Ensure that ContainerVolume '%s' has a corresponding Docker volume created...", vol.ObjectMeta.Name)
		_ = ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, vol)
	}

	t.Logf("Starting cleanup process...")
	req, reqCreationErr := http.NewRequestWithContext(ctx, "PATCH", adminDocUrl, nil)
	require.NoError(t, reqCreationErr)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Body = io.NopCloser(bytes.NewBufferString(`{"status":"CleaningResources"}`))
	req.Header.Set("Authorization", "Bearer "+serverInfo.ClientConfig.BearerToken)

	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to start resource cleanup")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	t.Logf("Waiting for API server to complete cleanup...")
	waitErr := ctrl_testutil.WaitApiServerStatus(ctx, client, serverInfo, apiserver.ApiServerCleanupComplete)
	require.NoError(t, waitErr, "Failed to wait for API server to complete cleanup")

	t.Logf("Verifying ContainerVolume objects were deleted...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &pVol)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &npVol)

	t.Logf("Ensure that volume associated with persistent ContainerVolume was preserved...")
	_, inspectedErr := serverInfo.ContainerOrchestrator.InspectVolumes(ctx, []string{pVol.Spec.Name})
	require.NoError(t, inspectedErr, "Could not ensure that the volume was not deleted")

	t.Logf("Ensure that volume associated with nonpersistent ContainerVolume was deleted...")
	notFoundErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, inspectionErr := serverInfo.ContainerOrchestrator.InspectVolumes(ctx, []string{testName})
		if inspectionErr != nil {
			if errors.Is(inspectionErr, containers.ErrNotFound) {
				return true, nil
			}

			return false, inspectionErr
		}

		return false, nil
	})
	require.NoError(t, notFoundErr, "Could not ensure that volume associated with nonpersistent ContainerVolume was deleted")
}

// Ensure that ContainerVolume behaves correctly when the container runtime is unhealthy.
func TestContainerVolumeRuntimeUnhealthy(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	const testName = "container-volume-runtime-unhealthy"
	log := testutil.NewLogForTesting(t.Name())

	// We are going to use a separate instance of the API server because we need to simulate container runtime being unhealthy,
	// and that might interfere with other tests if we used the shared container orchestrator.

	serverInfo, _, _, startupErr := StartTestEnvironment(ctx, VolumeController, t.Name(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: testName,
		},
	}

	t.Logf("Setting container runtime to unhealthy...")
	serverInfo.ContainerOrchestrator.SetRuntimeHealth(false)

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Logf("Ensure that ContainerVolume '%s' is marked as unhealthy...", vol.ObjectMeta.Name)
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, ctrl_client.ObjectKeyFromObject(&vol), func(updatedVol *apiv1.ContainerVolume) (bool, error) {
		return updatedVol.Status.State == apiv1.ContainerVolumeStateRuntimeUnhealthy, nil
	})

	t.Logf("Setting container runtime to healthy...")
	serverInfo.ContainerOrchestrator.SetRuntimeHealth(true)

	t.Logf("Ensure that ContainerVolume '%s' has a corresponding Docker volume created...", vol.ObjectMeta.Name)
	_ = ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, &vol)
}
