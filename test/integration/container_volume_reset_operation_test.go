/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type gatedVolumeResetOrchestrator struct {
	containers.ContainerOrchestrator
	phase   string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (o *gatedVolumeResetOrchestrator) waitForReset(ctx context.Context) error {
	o.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.release:
		return nil
	}
}

func (o *gatedVolumeResetOrchestrator) ListContainers(ctx context.Context, options containers.ListContainersOptions) ([]containers.ListedContainer, error) {
	if o.phase == "preflight" && options.All && len(options.Filters.LabelFilters) == 0 && o.armed.Swap(false) {
		if waitErr := o.waitForReset(ctx); waitErr != nil {
			return nil, waitErr
		}
	}
	return o.ContainerOrchestrator.ListContainers(ctx, options)
}

func (o *gatedVolumeResetOrchestrator) CreateVolume(ctx context.Context, options containers.CreateVolumeOptions) error {
	if o.phase == "recreation" && o.armed.Swap(false) {
		if waitErr := o.waitForReset(ctx); waitErr != nil {
			return waitErr
		}
	}
	return o.ContainerOrchestrator.CreateVolume(ctx, options)
}

// Verifies that operation deletion cancels preflight or recreation, releases its finalizer,
// and allows the same Container to resume with preserved or repaired owned storage.
func TestContainerVolumeResetOperationCancellation(t *testing.T) {
	for _, phase := range []string{"preflight", "recreation"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var gate *gatedVolumeResetOrchestrator
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, ContainerController|VolumeController,
				"ResetOperationCancel"+phase, t.TempDir(), TestEnvironmentOptions{
					DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
						gate = &gatedVolumeResetOrchestrator{
							ContainerOrchestrator: orchestrator, phase: phase,
							entered: make(chan struct{}, 1), release: make(chan struct{}, 1),
						}
						return gate
					},
				})
			require.NoError(t, startErr)
			volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"}, Spec: apiv1.ContainerVolumeSpec{Name: "reset-volume"}}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			target, originalRuntime := createVolumeResetContainer(t, ctx, server, false,
				[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}})
			gate.armed.Store(true)
			reset := &apiv1.ContainerVolumeReset{
				ObjectMeta: metav1.ObjectMeta{Name: "cancel-reset"},
				Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: target.Name, ContainerUID: target.UID},
			}
			require.NoError(t, server.Client.Create(ctx, reset))
			select {
			case <-gate.entered:
			case <-ctx.Done():
				require.NoError(t, ctx.Err(), "waiting for reset runtime boundary")
			}
			require.NoError(t, server.Client.Delete(ctx, reset))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, reset)
			resumed, runtime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
			require.Equal(t, target.UID, resumed.UID)
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, before.Labels, after.Labels)
			if phase == "preflight" {
				require.Equal(t, originalRuntime.Id, runtime.Id)
				require.Equal(t, before.CreatedAt, after.CreatedAt)
			} else {
				require.NotEqual(t, originalRuntime.Id, runtime.Id)
				require.True(t, after.CreatedAt.After(before.CreatedAt))
			}
			retry := submitVolumeReset(t, ctx, server.Client, resumed)
			require.Equal(t, "Succeeded", retry.Status.State, retry.Status.Message)
		})
	}
}

// Verifies that an overlapping reset is refused while another request is running rather than
// silently executing a second reset after the first finishes.
func TestContainerVolumeResetOperationOverlap(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var gate *gatedVolumeResetOrchestrator
	base := time.Now().UTC().Truncate(time.Microsecond)
	var elapsed atomic.Int64
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, ContainerController|VolumeController,
		"ResetOperationOverlap", t.TempDir(), TestEnvironmentOptions{
			VolumeResetClock: func() time.Time { return base.Add(time.Duration(elapsed.Load())) },
			DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
				gate = &gatedVolumeResetOrchestrator{
					ContainerOrchestrator: orchestrator, phase: "preflight",
					entered: make(chan struct{}, 1), release: make(chan struct{}, 1),
				}
				return gate
			},
		})
	require.NoError(t, startErr)
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"}, Spec: apiv1.ContainerVolumeSpec{Name: "reset-volume"}}
	require.NoError(t, server.Client.Create(ctx, volume))
	_ = ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	target, _ := createVolumeResetContainer(t, ctx, server, false,
		[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}})
	gate.armed.Store(true)
	first := &apiv1.ContainerVolumeReset{
		ObjectMeta: metav1.ObjectMeta{Name: "first-reset"},
		Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: target.Name, ContainerUID: target.UID},
	}
	require.NoError(t, server.Client.Create(ctx, first))
	select {
	case <-gate.entered:
	case <-ctx.Done():
		require.NoError(t, ctx.Err(), "waiting for first reset")
	}
	elapsed.Store(int64(3 * controllers.ContainerVolumeResetTerminalRetention))
	duplicate := &apiv1.ContainerVolumeReset{ObjectMeta: metav1.ObjectMeta{Name: first.Name}, Spec: first.Spec}
	require.True(t, apierrors.IsAlreadyExists(server.Client.Create(ctx, duplicate)))
	inFlight := &apiv1.ContainerVolumeReset{}
	require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(first), inFlight))
	require.Equal(t, first.UID, inFlight.UID)
	require.Equal(t, "Running", inFlight.Status.State)
	require.True(t, inFlight.DeletionTimestamp.IsZero())
	require.True(t, inFlight.Status.FinishTimestamp.IsZero())
	second := submitVolumeReset(t, ctx, server.Client, target)
	require.Equal(t, "Failed", second.Status.State)
	require.Contains(t, second.Status.Message, "active reset operation")
	require.False(t, second.Status.ContainerRemoved)
	gate.release <- struct{}{}
	completed := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(first), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
		return updated.Status.State == "Succeeded" || updated.Status.State == "Failed", nil
	})
	require.Equal(t, "Succeeded", completed.Status.State, completed.Status.Message)
	require.Equal(t, base.Add(3*controllers.ContainerVolumeResetTerminalRetention), completed.Status.FinishTimestamp.Time.UTC())
	resumed, _ := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
	require.Equal(t, target.UID, resumed.UID)
	unchanged := &apiv1.ContainerVolumeReset{}
	require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(second), unchanged))
	require.Equal(t, second.Status, unchanged.Status)
}

// Verifies that reset operations reject replaced or missing Container identities and immutable
// target changes through the real API without destroying the current physical container.
func TestContainerVolumeResetOperationIdentity(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx, ContainerController|VolumeController,
		"ResetOperationIdentity", t.TempDir())
	require.NoError(t, startErr)
	target, originalRuntime := createVolumeResetContainer(t, ctx, server, false, nil)
	reset := &apiv1.ContainerVolumeReset{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-reset"},
		Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: target.Name, ContainerUID: "old-api-uid"},
	}
	require.NoError(t, server.Client.Create(ctx, reset))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(reset), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
		return updated.Status.State == "Failed", nil
	})
	require.Contains(t, failed.Status.Message, "deleted or replaced")
	require.False(t, failed.Status.ContainerRemoved)
	patch := ctrl_client.MergeFrom(failed.DeepCopy())
	failed.Spec.ContainerUID = target.UID
	require.Error(t, server.Client.Patch(ctx, failed, patch))
	resumed, runtime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
	require.Equal(t, originalRuntime.Id, runtime.Id)
	require.Equal(t, target.UID, resumed.UID)
	existing := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-target"},
		Spec:       apiv1.ContainerSpec{ContainerName: target.Spec.ContainerName, Mode: apiv1.ContainerModeExisting},
	}
	require.NoError(t, server.Client.Create(ctx, existing))
	adopted, _ := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, existing)
	unsupported := submitVolumeReset(t, ctx, server.Client, adopted)
	require.Equal(t, "Failed", unsupported.Status.State)
	require.Contains(t, unsupported.Status.Message, "only session and persistent")
	require.False(t, unsupported.Status.ContainerRemoved)
	missing := &apiv1.ContainerVolumeReset{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-reset"},
		Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: "missing", ContainerUID: "missing-uid"},
	}
	require.NoError(t, server.Client.Create(ctx, missing))
	missingFailed := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(missing), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
		return updated.Status.State == "Failed", nil
	})
	require.Contains(t, missingFailed.Status.Message, "deleted or replaced")
}

// Verifies that deleting the target during preflight fails the operation without resetting
// storage and that the terminal operation cannot act on a replacement Container API resource.
func TestContainerVolumeResetOperationTargetDeletion(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var gate *gatedVolumeResetOrchestrator
	server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, ContainerController|VolumeController,
		"ResetOperationTargetDeletion", t.TempDir(), TestEnvironmentOptions{
			DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
				gate = &gatedVolumeResetOrchestrator{
					ContainerOrchestrator: orchestrator, phase: "preflight",
					entered: make(chan struct{}, 1), release: make(chan struct{}, 1),
				}
				return gate
			},
		})
	require.NoError(t, startErr)
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"}, Spec: apiv1.ContainerVolumeSpec{Name: "reset-volume"}}
	require.NoError(t, server.Client.Create(ctx, volume))
	before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	target, originalRuntime := createVolumeResetContainer(t, ctx, server, true,
		[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}})
	gate.armed.Store(true)
	reset := &apiv1.ContainerVolumeReset{
		ObjectMeta: metav1.ObjectMeta{Name: "deleted-target-reset"},
		Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: target.Name, ContainerUID: target.UID},
	}
	require.NoError(t, server.Client.Create(ctx, reset))
	select {
	case <-gate.entered:
	case <-ctx.Done():
		require.NoError(t, ctx.Err(), "waiting for reset preflight")
	}
	require.NoError(t, server.Client.Delete(ctx, target))
	gate.release <- struct{}{}
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(reset), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
		return updated.Status.State == "Failed", nil
	})
	require.False(t, failed.Status.ContainerRemoved)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, target)
	replacement, runtime := createVolumeResetContainer(t, ctx, server, true, target.Spec.VolumeMounts)
	require.NotEqual(t, target.UID, replacement.UID)
	require.Equal(t, originalRuntime.Id, runtime.Id)
	after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	require.Equal(t, before.CreatedAt, after.CreatedAt)
	unchanged := &apiv1.ContainerVolumeReset{}
	require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(failed), unchanged))
	require.Equal(t, failed.Status, unchanged.Status)
}

// Verifies that resetting a Container built from a Dockerfile reenters image build and startup
// without losing its generated image name or changing the Container API identity.
func TestContainerVolumeResetBuiltContainer(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx, ContainerController|VolumeController,
		"ResetBuiltContainer", t.TempDir())
	require.NoError(t, startErr)
	volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"}, Spec: apiv1.ContainerVolumeSpec{Name: "reset-volume"}}
	require.NoError(t, server.Client.Create(ctx, volume))
	_ = ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
	target := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{Name: "built-reset-target"},
		Spec: apiv1.ContainerSpec{
			ContainerName: "built-reset-target",
			Build:         &apiv1.ContainerBuildContext{Context: ".", Dockerfile: "./Dockerfile"},
			VolumeMounts:  []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}},
		},
	}
	require.NoError(t, server.Client.Create(ctx, target))
	running, originalRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
	reset := submitVolumeReset(t, ctx, server.Client, running)
	require.Equal(t, "Succeeded", reset.Status.State, reset.Status.Message)
	resumed, runtime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, running)
	require.Equal(t, running.UID, resumed.UID)
	require.NotEqual(t, originalRuntime.Id, runtime.Id)
	require.Equal(t, originalRuntime.Image, runtime.Image)
	require.Empty(t, resumed.Status.Message)
	require.True(t, resumed.Status.FinishTimestamp.IsZero())
}
