/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/apiserver"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/testutil"
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
		inspectedVolumes, err := vo.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{volume.Spec.Name},
		})
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

func TestPersistentVolumeRecordsWorkloadID(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	serverInfo, teInfo, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "PersistentVolumeWorkloadID", t.TempDir(), TestEnvironmentOptions{
		WorkloadID: "workload-a",
	})
	require.NoError(t, envStartErr)

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "persistent-volume-workload-id",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: "persistent-volume-workload-id",
		},
	}
	require.NoError(t, teInfo.StateStore.DeletePersistentVolume(ctx, vol.GetLeaseKey()))
	require.NoError(t, serverInfo.Client.Create(ctx, &vol))

	inspectedVolume := ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, &vol)

	record, getErr := teInfo.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
	require.NoError(t, getErr)
	require.Equal(t, commonapi.WorkloadID("workload-a"), record.WorkloadID)
	require.Equal(t, inspectedVolume.Name, record.VolumeName)
	require.NotEmpty(t, record.OwnershipToken)
	require.Equal(t, record.OwnershipToken, inspectedVolume.Labels[containers.VolumeOwnershipTokenLabel])
}

func TestExistingPersistentVolumeIsNotRecordedForWorkloadCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	serverInfo, teInfo, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "ExistingPersistentVolumeWorkloadID", t.TempDir(), TestEnvironmentOptions{
		WorkloadID: "workload-a",
	})
	require.NoError(t, envStartErr)

	vol := apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-persistent-volume-workload-id",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{
			Name: "existing-persistent-volume-workload-id",
		},
	}
	require.NoError(t, teInfo.StateStore.DeletePersistentVolume(ctx, vol.GetLeaseKey()))
	require.NoError(t, serverInfo.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: vol.Spec.Name}))
	require.NoError(t, serverInfo.Client.Create(ctx, &vol))

	_ = ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, &vol)

	_, getErr := teInfo.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
	require.ErrorIs(t, getErr, statestore.ErrPersistentVolumeNotFound)
}

func TestPersistentVolumeRecordPrecedesRuntimeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	var recordingOrchestrator *recordingVolumeCreateOrchestrator
	serverInfo, teInfo, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "PersistentVolumeRecordBeforeCreate", t.TempDir(), TestEnvironmentOptions{
		WorkloadID: "workload-a",
		DecorateContainerOrchestrator: func(
			orchestrator containers.ContainerOrchestrator,
			stateStore *statestore.Store,
		) containers.ContainerOrchestrator {
			recordingOrchestrator = &recordingVolumeCreateOrchestrator{
				ContainerOrchestrator: orchestrator,
				stateStore:            stateStore,
			}
			return recordingOrchestrator
		},
	})
	require.NoError(t, envStartErr)

	volume := persistentVolumeForTest("persistent-volume-record-before-create")
	require.NoError(t, serverInfo.Client.Create(ctx, volume))

	inspectedVolume := ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, volume)
	require.True(t, recordingOrchestrator.recordMatchedCreate.Load())
	record, getRecordErr := teInfo.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.NoError(t, getRecordErr)
	require.Equal(t, record.OwnershipToken, inspectedVolume.Labels[containers.VolumeOwnershipTokenLabel])
}

func TestPersistentVolumePersistenceFailurePreventsRuntimeCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	var failingOrchestrator *volumePersistenceFailureOrchestrator
	serverInfo, _, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "PersistentVolumePersistenceFailure", t.TempDir(), TestEnvironmentOptions{
		WorkloadID: "workload-a",
		DecorateContainerOrchestrator: func(
			orchestrator containers.ContainerOrchestrator,
			stateStore *statestore.Store,
		) containers.ContainerOrchestrator {
			failingOrchestrator = &volumePersistenceFailureOrchestrator{
				ContainerOrchestrator: orchestrator,
				stateStore:            stateStore,
				persistenceFailed:     make(chan struct{}),
			}
			return failingOrchestrator
		},
	})
	require.NoError(t, envStartErr)

	volume := persistentVolumeForTest("persistent-volume-persistence-failure")
	require.NoError(t, serverInfo.Client.Create(ctx, volume))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, ctrl_client.ObjectKeyFromObject(volume), func(updatedVolume *apiv1.ContainerVolume) (bool, error) {
		return updatedVolume.Status.State == apiv1.ContainerVolumeStatePending, nil
	})

	select {
	case <-failingOrchestrator.persistenceFailed:
	case <-ctx.Done():
		require.FailNow(t, "state store persistence failure was not injected", ctx.Err())
	}
	require.NoError(t, failingOrchestrator.closeErr)
	require.False(t, failingOrchestrator.createCalled.Load())
	_, inspectErr := serverInfo.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volume.Spec.Name}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

func TestPersistentVolumeWithoutWorkloadIDDoesNotUseStateStore(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	serverInfo, _, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "PersistentVolumeWithoutWorkloadID", t.TempDir(), TestEnvironmentOptions{
		DecorateContainerOrchestrator: func(
			orchestrator containers.ContainerOrchestrator,
			stateStore *statestore.Store,
		) containers.ContainerOrchestrator {
			require.NoError(t, stateStore.Close())
			return orchestrator
		},
	})
	require.NoError(t, envStartErr)

	volume := persistentVolumeForTest("persistent-volume-without-workload-id")
	require.NoError(t, serverInfo.Client.Create(ctx, volume))
	_ = ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, volume)
}

func TestPersistentVolumeCreateRaceAdoptsUnlabeledVolume(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	serverInfo, teInfo, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "PersistentVolumeCreateRace", t.TempDir(), TestEnvironmentOptions{
		WorkloadID: "workload-a",
		DecorateContainerOrchestrator: func(
			orchestrator containers.ContainerOrchestrator,
			_ *statestore.Store,
		) containers.ContainerOrchestrator {
			return &idempotentVolumeCreateRaceOrchestrator{ContainerOrchestrator: orchestrator}
		},
	})
	require.NoError(t, envStartErr)

	volume := persistentVolumeForTest("persistent-volume-create-race")
	require.NoError(t, serverInfo.Client.Create(ctx, volume))

	inspectedVolume := ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, volume)
	require.Empty(t, inspectedVolume.Labels)
	_, getRecordErr := teInfo.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.ErrorIs(t, getRecordErr, statestore.ErrPersistentVolumeNotFound)
}

func TestPersistentVolumeAmbiguousCreateFailureRetainsOwnershipRecord(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	serverInfo, teInfo, envStartErr := StartTestEnvironmentWithOptions(ctx, VolumeController, "PersistentVolumeAmbiguousCreate", t.TempDir(), TestEnvironmentOptions{
		WorkloadID: "workload-a",
		DecorateContainerOrchestrator: func(
			orchestrator containers.ContainerOrchestrator,
			_ *statestore.Store,
		) containers.ContainerOrchestrator {
			return &ambiguousVolumeCreateOrchestrator{ContainerOrchestrator: orchestrator}
		},
	})
	require.NoError(t, envStartErr)

	volume := persistentVolumeForTest("persistent-volume-ambiguous-create")
	require.NoError(t, serverInfo.Client.Create(ctx, volume))

	inspectedVolume := ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, volume)
	record, getRecordErr := teInfo.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
	require.NoError(t, getRecordErr)
	require.Equal(t, record.OwnershipToken, inspectedVolume.Labels[containers.VolumeOwnershipTokenLabel])
}

func persistentVolumeForTest(name string) *apiv1.ContainerVolume {
	return &apiv1.ContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerVolumeSpec{Name: name},
	}
}

type recordingVolumeCreateOrchestrator struct {
	containers.ContainerOrchestrator
	stateStore          *statestore.Store
	recordMatchedCreate atomic.Bool
}

func (o *recordingVolumeCreateOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	record, getRecordErr := o.stateStore.GetPersistentVolume(ctx, "containervolumes/"+options.Name)
	if getRecordErr == nil &&
		record.OwnershipToken != "" &&
		record.OwnershipToken == options.Labels[containers.VolumeOwnershipTokenLabel] {
		o.recordMatchedCreate.Store(true)
	}
	return o.ContainerOrchestrator.CreateVolume(ctx, options)
}

type volumePersistenceFailureOrchestrator struct {
	containers.ContainerOrchestrator
	stateStore        *statestore.Store
	closeOnce         sync.Once
	closeErr          error
	persistenceFailed chan struct{}
	createCalled      atomic.Bool
}

func (o *volumePersistenceFailureOrchestrator) CheckStatus(
	ctx context.Context,
	usage containers.CachedRuntimeStatusUsage,
) containers.ContainerRuntimeStatus {
	o.closeOnce.Do(func() {
		o.closeErr = o.stateStore.Close()
		close(o.persistenceFailed)
	})
	return o.ContainerOrchestrator.CheckStatus(ctx, usage)
}

func (o *volumePersistenceFailureOrchestrator) CreateVolume(
	ctx context.Context,
	options containers.CreateVolumeOptions,
) error {
	o.createCalled.Store(true)
	return o.ContainerOrchestrator.CreateVolume(ctx, options)
}

type idempotentVolumeCreateRaceOrchestrator struct {
	containers.ContainerOrchestrator
	initialInspect atomic.Bool
}

func (o *idempotentVolumeCreateRaceOrchestrator) InspectVolumes(
	ctx context.Context,
	options containers.InspectVolumesOptions,
) ([]containers.InspectedVolume, error) {
	if o.initialInspect.CompareAndSwap(false, true) {
		return nil, containers.ErrNotFound
	}
	return o.ContainerOrchestrator.InspectVolumes(ctx, options)
}

func (o *idempotentVolumeCreateRaceOrchestrator) CreateVolume(
	ctx context.Context,
	options containers.CreateVolumeOptions,
) error {
	externalCreateErr := o.ContainerOrchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: options.Name})
	if externalCreateErr != nil && !errors.Is(externalCreateErr, containers.ErrAlreadyExists) {
		return externalCreateErr
	}
	return nil
}

type ambiguousVolumeCreateOrchestrator struct {
	containers.ContainerOrchestrator
	failureInjected atomic.Bool
}

func (o *ambiguousVolumeCreateOrchestrator) CreateVolume(
	ctx context.Context,
	options containers.CreateVolumeOptions,
) error {
	if !o.failureInjected.CompareAndSwap(false, true) {
		return o.ContainerOrchestrator.CreateVolume(ctx, options)
	}
	createErr := o.ContainerOrchestrator.CreateVolume(ctx, options)
	if createErr != nil {
		return createErr
	}
	return backoff.Permanent(errors.New("volume create result unavailable"))
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

	_, inspectedErr := containerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{testName},
	})
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
		_, inspectionErr := containerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{testName},
		})
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

	serverInfo, _, startupErr := StartTestEnvironment(ctx, VolumeController, t.Name(), NoSeparateWorkingDir)
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
	_, inspectedErr := serverInfo.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{pVol.Spec.Name},
	})
	require.NoError(t, inspectedErr, "Could not ensure that the volume was not deleted")

	t.Logf("Ensure that volume associated with nonpersistent ContainerVolume was deleted...")
	notFoundErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, inspectionErr := serverInfo.ContainerOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{testName},
		})
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

	// We are going to use a separate instance of the API server because we need to simulate container runtime being unhealthy,
	// and that might interfere with other tests if we used the shared container orchestrator.

	serverInfo, _, startupErr := StartTestEnvironment(ctx, VolumeController, t.Name(), NoSeparateWorkingDir)
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
	tco, isTCO := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTCO, "Container orchestrator should be a TestContainerOrchestrator")
	tco.SetRuntimeHealth(false)

	t.Logf("Creating ContainerVolume object '%s'...", vol.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &vol)
	require.NoError(t, err, "Could not create a ContainerVolume object")

	t.Logf("Ensure that ContainerVolume '%s' is marked as unhealthy...", vol.ObjectMeta.Name)
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, ctrl_client.ObjectKeyFromObject(&vol), func(updatedVol *apiv1.ContainerVolume) (bool, error) {
		return updatedVol.Status.State == apiv1.ContainerVolumeStateRuntimeUnhealthy, nil
	})

	t.Logf("Setting container runtime to healthy...")
	tco.SetRuntimeHealth(true)

	t.Logf("Ensure that ContainerVolume '%s' has a corresponding Docker volume created...", vol.ObjectMeta.Name)
	_ = ensureVolumeCreated(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, &vol)
}
