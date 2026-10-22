/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that retention starts at publication rather than completion, duplicate submissions
// and status updates never repeat execution, and name reuse after expiry creates a new operation.
func TestContainerVolumeResetTerminalRetention(t *testing.T) {
	for _, outcome := range []string{"Succeeded", "Failed"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			base := time.Now().UTC().Truncate(time.Microsecond)
			publicationDelay := 3 * controllers.ContainerVolumeResetTerminalRetention
			var elapsed atomic.Int64
			elapsed.Store(int64(publicationDelay))
			var clockReads atomic.Int64
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, ContainerController|VolumeController,
				"ResetRetention"+outcome, t.TempDir(), TestEnvironmentOptions{
					VolumeResetClock: func() time.Time {
						if clockReads.Add(1) == 1 {
							return base
						}
						return base.Add(time.Duration(elapsed.Load()))
					},
				})
			require.NoError(t, startErr)
			volume := &apiv1.ContainerVolume{ObjectMeta: metav1.ObjectMeta{Name: "reset-volume"}, Spec: apiv1.ContainerVolumeSpec{Name: "reset-volume"}}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			target, originalRuntime := createVolumeResetContainer(t, ctx, server, false,
				[]apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: volume.Spec.Name, Target: "/data"}})
			resetTarget := target.DeepCopy()
			if outcome == "Failed" {
				resetTarget.UID = "replaced-target-uid"
			}
			completed := submitVolumeReset(t, ctx, server.Client, resetTarget)
			require.Equal(t, outcome, completed.Status.State, completed.Status.Message)
			waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(completed), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
				return updated.Status.State == outcome && clockReads.Load() >= 3, nil
			})
			require.Equal(t, base, completed.Status.FinishTimestamp.Time.UTC())
			_, resumedRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			if outcome == "Succeeded" {
				require.NotEqual(t, originalRuntime.Id, resumedRuntime.Id)
				require.True(t, after.CreatedAt.After(before.CreatedAt))
			} else {
				require.Equal(t, originalRuntime.Id, resumedRuntime.Id)
				require.Equal(t, before.CreatedAt, after.CreatedAt)
			}

			duplicate := &apiv1.ContainerVolumeReset{ObjectMeta: metav1.ObjectMeta{Name: completed.Name}, Spec: completed.Spec}
			require.True(t, apierrors.IsAlreadyExists(server.Client.Create(ctx, duplicate)))
			existing := &apiv1.ContainerVolumeReset{}
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(completed), existing))
			require.Equal(t, completed.UID, existing.UID)
			require.Equal(t, completed.Spec, existing.Spec)
			require.Equal(t, completed.Status, existing.Status)

			statusPatch := ctrl_client.MergeFrom(existing.DeepCopy())
			existing.Status.State = "Pending"
			require.NoError(t, server.Client.Status().Patch(ctx, existing, statusPatch))
			restored := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(completed), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
				return updated.Status.State == outcome, nil
			})
			require.Equal(t, completed.Status, restored.Status)
			_, unchangedRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
			require.Equal(t, resumedRuntime.Id, unchangedRuntime.Id)
			unchangedVolume := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, after.CreatedAt, unchangedVolume.CreatedAt)

			elapsed.Store(int64(publicationDelay + controllers.ContainerVolumeResetTerminalRetention - time.Second))
			readsBefore := clockReads.Load()
			beforeExpiryPatch := ctrl_client.MergeFrom(restored.DeepCopy())
			restored.Annotations = map[string]string{"retention-check": "before-expiry"}
			require.NoError(t, server.Client.Patch(ctx, restored, beforeExpiryPatch))
			retained := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(completed), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
				return clockReads.Load() > readsBefore, nil
			})
			require.True(t, retained.DeletionTimestamp.IsZero())
			require.Equal(t, completed.Status, retained.Status)

			elapsed.Store(int64(publicationDelay + controllers.ContainerVolumeResetTerminalRetention))
			expiryPatch := ctrl_client.MergeFrom(retained.DeepCopy())
			retained.Annotations["retention-check"] = "at-expiry"
			require.NoError(t, server.Client.Patch(ctx, retained, expiryPatch))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, completed)
			require.True(t, apierrors.IsNotFound(server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(completed), &apiv1.ContainerVolumeReset{})))
			finalVolume := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, after.CreatedAt, finalVolume.CreatedAt)

			reused := &apiv1.ContainerVolumeReset{ObjectMeta: metav1.ObjectMeta{Name: completed.Name}, Spec: completed.Spec}
			require.NoError(t, server.Client.Create(ctx, reused))
			require.NotEqual(t, completed.UID, reused.UID)
			next := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(reused), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
				return updated.Status.State == outcome, nil
			})
			if outcome == "Succeeded" {
				_, nextRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
				require.NotEqual(t, resumedRuntime.Id, nextRuntime.Id)
			}
			require.NoError(t, server.Client.Delete(ctx, next))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, next)
		})
	}
}
