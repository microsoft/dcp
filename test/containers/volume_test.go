/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	std_slices "slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
)

func TestVolumeMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		volumeName := containertest.UniqueName(t, "volume")
		require.NoError(t, tracker.TrackVolume(volumeName))
		createErr := runtime.Orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{
			Name:   volumeName,
			Labels: tracker.MapLabels(),
		})
		require.NoError(t, createErr)

		inspected, inspectErr := runtime.Orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{volumeName},
		})
		require.NoError(t, inspectErr)
		require.Len(t, inspected, 1)
		require.Equal(t, volumeName, inspected[0].Name)
		require.Equal(t, tracker.RunID(), inspected[0].Labels[containertest.TestRunLabel])

		listed, listErr := runtime.Orchestrator.ListVolumes(ctx, containers.ListVolumesOptions{
			Filters: containers.ListVolumesFilters{
				LabelFilters: []containers.LabelFilter{{
					Key:   containertest.TestRunLabel,
					Value: tracker.RunID(),
				}},
			},
		})
		require.NoError(t, listErr)
		require.NotEqual(t, -1, std_slices.IndexFunc(listed, func(volume containers.ListedVolume) bool {
			return volume.Name == volumeName
		}))

		containerName := containertest.UniqueName(t, "volume-container")
		require.NoError(t, tracker.TrackContainer(containerName))
		containerID, runErr := runtime.Orchestrator.RunContainer(ctx, containers.RunContainerOptions{
			CreateContainerOptions: containers.CreateContainerOptions{
				Name:       containerName,
				Image:      ensureBaseImage(t, ctx, runtime),
				Command:    []string{"sh", "-c", `printf "volume-content" > /data/value; exec sleep 600`},
				Labels:     tracker.Labels(),
				PullPolicy: containers.PullPolicyNever,
				VolumeMounts: []containers.CreateContainerVolumeMount{{
					Type:   containers.NamedVolumeMount,
					Source: volumeName,
					Target: "/data",
				}},
			},
		})
		require.NoError(t, runErr)
		waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusRunning)

		exitCode, stdout, stderr := execContainer(t, ctx, runtime.Orchestrator, containers.ExecContainerOptions{
			Container: containerID,
			Command:   "sh",
			Args:      []string{"-c", `while [ ! -f /data/value ]; do sleep 1; done; cat /data/value`},
		})
		require.Equal(t, int32(0), exitCode)
		require.Equal(t, "volume-content", stdout)
		require.Empty(t, stderr)

		_, removeContainerErr := runtime.Orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
			Force:      true,
		})
		require.NoError(t, removeContainerErr)
		waitForContainerAbsent(t, ctx, runtime.Orchestrator, containerID)

		removed, removeErr := runtime.Orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
			Volumes: []string{volumeName},
		})
		require.NoError(t, removeErr)
		require.Equal(t, []string{volumeName}, removed)
		waitForVolumeAbsent(t, ctx, runtime.Orchestrator, volumeName)
	})
}
