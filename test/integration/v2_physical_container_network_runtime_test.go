/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	std_slices "slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestPhysicalContainerNetworkRemovesStoppedAttachmentsWithRealOrchestrator(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		ctx, cancel := context.WithCancel(runtimeCtx)
		defer cancel()
		tracker := containertest.NewResourceTracker(t, runtime)
		networkName := containertest.UniqueName(t, "physical-network-cleanup")
		containerName := containertest.UniqueName(t, "physical-network-stopped")
		require.NoError(t, tracker.TrackNetwork(networkName))
		require.NoError(t, tracker.TrackContainer(containerName))

		serverInfo, environmentInfo, startupErr := StartAdvancedTestEnvironmentWithOptions(
			ctx,
			NamespaceController|PhysicalContainerNetworkController,
			containertest.UniqueName(t, "physical-network-environment"),
			t.TempDir(),
			AdvancedTestEnvironmentOptions{
				ApiServerFlags:        ctrl_testutil.ApiServerUseTrueContainerOrchestrator,
				ContainerOrchestrator: runtime.Orchestrator,
			},
		)
		require.NoError(t, startupErr)
		defer environmentInfo.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, ctx, cancel, serverInfo)

		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: containertest.UniqueName(t, "physical-network-namespace"),
		}}
		require.NoError(t, serverInfo.Client.Create(ctx, namespace))
		waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(updated *apiv2.Namespace) (bool, error) {
			return updated.Status.Phase == apiv2.NamespacePhaseActive, nil
		})
		networkResource := &apiv2.PhysicalContainerNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
			Spec: apiv2.PhysicalContainerNetworkSpec{
				Network: &apiv2.PhysicalContainerNetworkConfig{
					NetworkName: networkName,
					Labels:      tracker.Labels(),
				},
			},
		}
		require.NoError(t, serverInfo.Client.Create(ctx, networkResource))
		readyNetwork := waitPhysicalContainerNetworkPhaseEx(
			t, ctx, serverInfo.Client, networkResource.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseReady,
		)
		networkID := readyNetwork.Status.NetworkID

		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{
			Image: "mcr.microsoft.com/azurelinux/distroless/minimal:3.0.20260809",
		})
		require.NoError(t, pullErr)

		const probePath = "/dcp-network-probe"
		containerID, createContainerErr := runtime.Orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
			Name: containerName, Image: imageID,
			Entrypoint: probePath, Command: []string{"exit"},
			Labels: tracker.Labels(), PullPolicy: containers.PullPolicyNever,
			Networks: []containers.CreateContainerNetworkOptions{{Name: networkName}},
		})
		require.NoError(t, createContainerErr)

		probeBinary, probePathErr := internal_testutil.GetTestContainerToolPath("container_probe_c")
		require.NoError(t, probePathErr)
		require.NoError(t, runtime.Orchestrator.CreateFiles(ctx, containers.CreateFilesOptions{
			Container: containerID, Destination: "/",
			Entries: []containers.FileSystemEntry{{
				Name: "dcp-network-probe", Source: probeBinary, Mode: 0755,
			}},
		}))
		_, startErr := runtime.Orchestrator.StartContainers(ctx, containers.StartContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, startErr)

		waitErr := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
			inspected, inspectErr := runtime.Orchestrator.InspectContainers(pollCtx, containers.InspectContainersOptions{
				Containers: []string{containerID},
			})
			if inspectErr != nil {
				return false, inspectErr
			}
			return len(inspected) == 1 && inspected[0].Status == containers.ContainerStatusExited, nil
		})
		require.NoError(t, waitErr)

		require.NoError(t, serverInfo.Client.Delete(ctx, networkResource))
		ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerNetwork](t, ctx, serverInfo.Client, networkResource)

		remainingNetworks, inspectNetworkErr := runtime.Orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		require.Empty(t, remainingNetworks)
		require.ErrorIs(t, inspectNetworkErr, containers.ErrNotFound)
		remainingContainers, inspectContainerErr := runtime.Orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, inspectContainerErr)
		require.Len(t, remainingContainers, 1)
		require.False(t, std_slices.ContainsFunc(remainingContainers[0].Networks, func(network containers.InspectedContainerNetwork) bool {
			return network.Id == networkID || network.Name == networkName
		}))
	})
}
