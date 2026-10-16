/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpclient"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func openV2ContainerLog[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context, obj PT, opts apiv2.ResourceLogOptions) (io.ReadCloser, error) {
	gvr := obj.GetGroupVersionResource()
	return restClient.Get().
		AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", obj.GetObjectMeta().Namespace, gvr.Resource, obj.GetObjectMeta().Name, apiv2.LogSubresourceName).
		SpecificallyVersionedParams(&opts, runtime.NewParameterCodec(dcpclient.NewScheme()), apiv2.GroupVersion).
		Stream(ctx)
}

func readV2ContainerLog[T commonapi.ObjectStruct, PT commonapi.PObjectStruct[T]](
	ctx context.Context, obj PT, opts apiv2.ResourceLogOptions) (string, error) {
	stream, streamErr := openV2ContainerLog(ctx, obj, opts)
	if streamErr != nil {
		return "", streamErr
	}
	contents, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	if readErr != nil {
		return "", readErr
	}
	return string(contents), closeErr
}

// Verifies that namespaced logical and physical log endpoints expose identical stdout and stderr,
// share physical capture identity, and reject unsupported startup sources before streaming.
func TestV2ContainerLogEndpointsShareRuntimeOutput(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-container-log-output")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
		}},
	}
	require.NoError(t, client.Create(ctx, container))
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.Equal(t, physical.UID, running.Status.PhysicalContainerUID)
	require.NoError(t, containerOrchestrator.SimulateContainerLogging(running.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("stdout-one\nstdout-two\n")))
	require.NoError(t, containerOrchestrator.SimulateContainerLogging(running.Status.ContainerID, apiv1.LogStreamSourceStderr, []byte("stderr-one\n")))
	logicalOut, logicalOutErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "stdout"})
	require.NoError(t, logicalOutErr)
	physicalOut, physicalOutErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "stdout"})
	require.NoError(t, physicalOutErr)
	require.Equal(t, "stdout-one\nstdout-two\n", logicalOut)
	require.Equal(t, logicalOut, physicalOut)
	logicalErrOutput, logicalErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "stderr"})
	require.NoError(t, logicalErr)
	physicalErrOutput, physicalErr := readV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Source: "stderr"})
	require.NoError(t, physicalErr)
	require.Equal(t, "stderr-one\n", logicalErrOutput)
	require.Equal(t, logicalErrOutput, physicalErrOutput)
	tail := int64(1)
	tailOutput, tailErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Tail: &tail, LineNumbers: true})
	require.NoError(t, tailErr)
	require.Contains(t, tailOutput, "stdout-two")
	require.NotContains(t, tailOutput, "stdout-one")
	startupStream, startupErr := openV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "startup_stdout"})
	if startupStream != nil {
		require.NoError(t, startupStream.Close())
	}
	require.True(t, apierrors.IsBadRequest(startupErr), "%v", startupErr)
}

// Verifies that closing a logical stdout consumer preserves the physical consumer,
// while deleting their API resources ends following even when the runtime container is retained.
func TestV2ContainerLogFollowersHaveIndependentLifetimes(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-container-log-follow")
	runtimeName := namespace.Name + "-runtime"
	runtimeID := runExistingTestContainer(t, ctx, runtimeName, "log-follow-image")
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerSpec{Mode: apiv2.LogicalContainerModeExisting, ContainerName: runtimeName},
	}
	require.NoError(t, client.Create(ctx, container))
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.NoError(t, containerOrchestrator.SimulateContainerLogging(runtimeID, apiv1.LogStreamSourceStdout, []byte("history\n")))
	logicalFollow, logicalFollowErr := openV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, logicalFollowErr)
	defer func() { require.NoError(t, logicalFollow.Close()) }()
	physicalFollow, physicalFollowErr := openV2ContainerLog(ctx, physical, apiv2.ResourceLogOptions{Follow: true})
	require.NoError(t, physicalFollowErr)
	defer func() { require.NoError(t, physicalFollow.Close()) }()
	history := make([]byte, len("history\n"))
	_, logicalHistoryErr := io.ReadFull(logicalFollow, history)
	require.NoError(t, logicalHistoryErr)
	require.Equal(t, "history\n", string(history))
	_, physicalHistoryErr := io.ReadFull(physicalFollow, history)
	require.NoError(t, physicalHistoryErr)
	require.Equal(t, "history\n", string(history))
	require.NoError(t, logicalFollow.Close())
	require.NoError(t, containerOrchestrator.SimulateContainerLogging(running.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("still-following\n")))
	nextLine := make([]byte, len("still-following\n"))
	_, nextLineErr := io.ReadFull(physicalFollow, nextLine)
	require.NoError(t, nextLineErr)
	require.Equal(t, "still-following\n", string(nextLine))
	require.NoError(t, client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, container)
	remaining, remainingErr := io.ReadAll(physicalFollow)
	require.NoError(t, remainingErr)
	require.Empty(t, remaining)
	retained, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{runtimeID}})
	require.NoError(t, inspectErr)
	require.Len(t, retained, 1)
	require.Equal(t, containers.ContainerStatusRunning, retained[0].Status)
}
