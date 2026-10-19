/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that logical network system followers receive later physical failures with producer
// provenance, remain independent, and end on logical deletion rather than on a Failed update.
func TestV2NetworkSystemLogsFollowPhysicalOwnership(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-network-log-owner")
	runtimeName := namespace.Name + "-runtime"
	removeRuntimeNetworkOnCleanup(t, runtimeName)
	_, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: runtimeName})
	require.NoError(t, createErr)
	releaseCreate := containerOrchestrator.BlockCreateNetwork(runtimeName)
	defer releaseCreate()
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{NetworkName: runtimeName, Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, client.Create(ctx, network))
	pending := waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.PhysicalNetworkRef != "", nil
	})
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: pending.Status.PhysicalNetworkRef}, physical))
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	first, firstErr := openV2ContainerLog(firstCtx, network, apiv2.ResourceLogOptions{Source: "system", Follow: true})
	require.NoError(t, firstErr)
	defer func() { require.NoError(t, first.Close()) }()
	second, secondErr := openV2ContainerLog(ctx, network, apiv2.ResourceLogOptions{Source: "system", Follow: true})
	require.NoError(t, secondErr)
	defer func() { require.NoError(t, second.Close()) }()
	physicalFollow, physicalFollowErr := openV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "system", Follow: true})
	require.NoError(t, physicalFollowErr)
	defer func() { require.NoError(t, physicalFollow.Close()) }()
	cancelFirst()
	require.NoError(t, first.Close())
	releaseCreate()
	waitObjectAssumesState(t, ctx, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed, nil
	})
	for _, stream := range []io.ReadCloser{second, physicalFollow} {
		reader := bufio.NewReader(stream)
		for {
			line, readErr := reader.ReadString('\n')
			require.NoError(t, readErr)
			if !strings.Contains(line, "Failed to create runtime network") {
				continue
			}
			require.Contains(t, line, runtimeName)
			require.Contains(t, line, "PhysicalContainerNetworkReconciler")
			break
		}
	}
	physicalSystem, physicalSystemErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, physicalSystemErr)
	logicalSystem, logicalSystemErr := readV2ContainerLog(ctx, network, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, logicalSystemErr)
	require.Contains(t, physicalSystem, runtimeName)
	require.Contains(t, logicalSystem, runtimeName)
	require.NotEqual(t, network.GetResourceId(), physical.GetResourceId())
	require.NoError(t, client.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, network)
	for _, stream := range []io.ReadCloser{second, physicalFollow} {
		_, readErr := io.ReadAll(stream)
		require.NoError(t, readErr)
	}
}

// Verifies that standalone physical-network endpoints preserve namespace/resource isolation
// and reject stdout, stderr, and startup streams rather than creating runtime log capture.
func TestV2NetworkLogEndpointsAreSystemOnlyAndNamespaced(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	for _, namespaceName := range []string{"v2-network-log-first", "v2-network-log-second"} {
		namespace := createActiveV2Namespace(t, ctx, namespaceName)
		runtimeName := namespaceName + "-runtime"
		removeRuntimeNetworkOnCleanup(t, runtimeName)
		_, createErr := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: runtimeName})
		require.NoError(t, createErr)
		physical := &apiv2.PhysicalContainerNetwork{
			ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
			Spec:       apiv2.PhysicalContainerNetworkSpec{Network: &apiv2.PhysicalContainerNetworkConfig{NetworkName: runtimeName}},
		}
		require.NoError(t, client.Create(ctx, physical))
		waitPhysicalContainerNetworkPhase(t, ctx, physical.NamespacedName(), apiv2.PhysicalContainerNetworkPhaseFailed)
		output, readErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "system"})
		require.NoError(t, readErr)
		require.Contains(t, output, runtimeName)
		require.Contains(t, output, "Failed to create runtime network")
		if namespaceName == "v2-network-log-second" {
			require.NotContains(t, output, "v2-network-log-first-runtime")
		}
		for _, source := range []string{"", "stdout", "stderr", "startup_stdout", "startup_stderr"} {
			stream, streamErr := openV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: source})
			if stream != nil {
				require.NoError(t, stream.Close())
			}
			require.True(t, apierrors.IsBadRequest(streamErr), "%v", streamErr)
		}
	}
}
