/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Stop during a real-runtime reset preserves stopped intent and empties storage,
// while deletion during preflight preserves persistent data and protects a replacement API identity.
func TestContainerVolumeResetRealRuntimeLifecycle(t *testing.T) {
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)
	dcppaths.EnableTestPathProbing()
	for _, scenario := range []struct {
		name       string
		persistent bool
		replace    bool
	}{
		{name: "stop-session"},
		{name: "stop-persistent", persistent: true},
		{name: "replace-persistent", persistent: true, replace: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var gate *gatedVolumeResetOrchestrator
			server, environment, startErr := StartAdvancedTestEnvironmentWithOptions(ctx, ContainerController|VolumeController,
				"ResetRuntimeLifecycle"+scenario.name, t.TempDir(), AdvancedTestEnvironmentOptions{
					ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator,
					DecorateContainerOrchestrator: func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
						gate = &gatedVolumeResetOrchestrator{
							ContainerOrchestrator: orchestrator, phase: "preflight",
							entered: make(chan struct{}, 1), release: make(chan struct{}, 1),
						}
						return gate
					},
				})
			require.NoError(t, startErr)
			defer environment.ProcessExecutor.Dispose()
			defer shutdownAdvancedTestEnvironment(t, ctx, cancel, server)
			suffix, suffixErr := randdata.MakeRandomString(8)
			require.NoError(t, suffixErr)
			resourceName := "reset-lifecycle-" + string(suffix)
			var runtimeIDs []string
			defer func() {
				for _, runtimeID := range runtimeIDs {
					_, containerCleanupErr := server.ContainerOrchestrator.RemoveContainers(context.Background(),
						containers.RemoveContainersOptions{Containers: []string{runtimeID}, Force: true})
					if containerCleanupErr != nil && !errors.Is(containerCleanupErr, containers.ErrNotFound) {
						t.Error(containerCleanupErr)
					}
				}
				_, volumeCleanupErr := server.ContainerOrchestrator.RemoveVolumes(context.Background(),
					containers.RemoveVolumesOptions{Volumes: []string{resourceName}})
				if volumeCleanupErr != nil && !errors.Is(volumeCleanupErr, containers.ErrNotFound) {
					t.Error(volumeCleanupErr)
				}
			}()
			persistent := scenario.persistent
			volume := &apiv1.ContainerVolume{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec:       apiv1.ContainerVolumeSpec{Name: resourceName, Persistent: &persistent},
			}
			require.NoError(t, server.Client.Create(ctx, volume))
			before := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			target := &apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName},
				Spec: apiv1.ContainerSpec{
					Image: "busybox:latest", ContainerName: resourceName, Persistent: persistent,
					Command: "sh", Args: []string{"-c", "sleep 600"},
					VolumeMounts: []apiv1.VolumeMount{{Type: apiv1.NamedVolumeMount, Source: resourceName, Target: "/data"}},
				},
			}
			require.NoError(t, server.Client.Create(ctx, target))
			running, originalRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, target)
			apiTarget := running
			runtimeIDs = append(runtimeIDs, originalRuntime.Id)
			runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, originalRuntime.Id, "printf marker > /data/marker")
			gate.armed.Store(true)
			reset := &apiv1.ContainerVolumeReset{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-reset"},
				Spec:       apiv1.ContainerVolumeResetSpec{ContainerName: running.Name, ContainerUID: running.UID},
			}
			require.NoError(t, server.Client.Create(ctx, reset))
			select {
			case <-gate.entered:
			case <-ctx.Done():
				require.NoError(t, ctx.Err(), "waiting for reset preflight")
			}
			if scenario.replace {
				require.NoError(t, server.Client.Delete(ctx, running))
			} else {
				stopPatch := ctrl_client.MergeFrom(running.DeepCopy())
				running.Spec.Stop = true
				require.NoError(t, server.Client.Patch(ctx, running, stopPatch))
			}
			gate.release <- struct{}{}
			completed := waitObjectAssumesStateEx(t, ctx, server.Client, ctrl_client.ObjectKeyFromObject(reset), func(updated *apiv1.ContainerVolumeReset) (bool, error) {
				return updated.Status.State == "Succeeded" || updated.Status.State == "Failed", nil
			})
			if scenario.replace {
				require.Equal(t, "Failed", completed.Status.State, completed.Status.Message)
				require.False(t, completed.Status.ContainerRemoved)
				require.Empty(t, completed.Status.Volumes)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, running)
				replacement := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: resourceName}, Spec: target.Spec}
				require.NoError(t, server.Client.Create(ctx, replacement))
				adopted, adoptedRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, replacement)
				apiTarget = adopted
				require.NotEqual(t, running.UID, adopted.UID)
				require.Equal(t, originalRuntime.Id, adoptedRuntime.Id)
				runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, adoptedRuntime.Id, "test -s /data/marker")
				unchangedVolume := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
				require.Equal(t, before.CreatedAt, unchangedVolume.CreatedAt)
				duplicate := &apiv1.ContainerVolumeReset{ObjectMeta: metav1.ObjectMeta{Name: reset.Name}, Spec: reset.Spec}
				require.True(t, apierrors.IsAlreadyExists(server.Client.Create(ctx, duplicate)))
				unchanged := &apiv1.ContainerVolumeReset{}
				require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(reset), unchanged))
				require.Equal(t, completed.UID, unchanged.UID)
				require.Equal(t, completed.Status, unchanged.Status)
				retry := submitVolumeReset(t, ctx, server.Client, adopted)
				require.Equal(t, "Succeeded", retry.Status.State, retry.Status.Message)
				_, recreatedRuntime := ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, adopted)
				runtimeIDs = []string{recreatedRuntime.Id}
				require.NotEqual(t, originalRuntime.Id, recreatedRuntime.Id)
				runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, recreatedRuntime.Id, "test -z \"$(ls -A /data)\"")
			} else {
				require.Equal(t, "Succeeded", completed.Status.State, completed.Status.Message)
				runtimeIDs = nil
				stopped := ensureContainerState(t, ctx, server.Client, running, apiv1.ContainerStateNotFound)
				require.Equal(t, running.UID, stopped.UID)
				require.True(t, stopped.Spec.Stop)
				require.Empty(t, stopped.Status.ContainerID)
				_, removedErr := server.ContainerOrchestrator.InspectContainers(ctx,
					containers.InspectContainersOptions{Containers: []string{originalRuntime.Id}})
				require.ErrorIs(t, removedErr, containers.ErrNotFound)
				probeID, probeCreateErr := server.ContainerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
					Name: resourceName + "-probe", Image: "busybox:latest", Command: []string{"sh", "-c", "sleep 600"},
					VolumeMounts: []containers.CreateContainerVolumeMount{
						{Type: containers.NamedVolumeMount, Source: resourceName, Target: "/data"},
					},
				})
				require.NoError(t, probeCreateErr)
				runtimeIDs = append(runtimeIDs, probeID)
				_, probeStartErr := server.ContainerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{Containers: []string{probeID}})
				require.NoError(t, probeStartErr)
				runVolumeResetCommand(t, ctx, server.ContainerOrchestrator, probeID, "test -z \"$(ls -A /data)\"")
			}
			after := ensureVolumeCreated(t, ctx, server.Client, server.ContainerOrchestrator, volume)
			require.Equal(t, before.Labels, after.Labels)
			require.True(t, after.CreatedAt.After(before.CreatedAt))
			require.NoError(t, server.Client.Delete(ctx, apiTarget))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, apiTarget)
		})
	}
}
