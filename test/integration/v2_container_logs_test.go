/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

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
// share physical capture identity, and reject invalid sources before streaming.
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
	require.Equal(t, physical.UID, running.Status.RuntimeLogs.UID)
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
	startupStream, startupErr := openV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "invalid-source"})
	if startupStream != nil {
		require.NoError(t, startupStream.Close())
	}
	require.True(t, apierrors.IsBadRequest(startupErr), "%v", startupErr)
}

// Verifies that owned image pull output follows before container readiness, completes after acquisition,
// and is identical through image and logical-container log endpoints.
func TestV2ContainerStartupLogsExposeOwnedImagePull(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-image-startup-pull")
	source := namespace.Name + "-source"
	releasePull := containerOrchestrator.BlockPullImage(source)
	defer releasePull()
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: source},
		}},
	}
	require.NoError(t, client.Create(ctx, container))
	waitPullImageCallCount(t, ctx, source, 1)
	pending := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.StartupLogs != nil, nil
	})
	require.Equal(t, apiv2.LogicalContainerPhasePending, pending.Status.Phase)
	follow, followErr := openV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true})
	require.NoError(t, followErr)
	defer func() { require.NoError(t, follow.Close()) }()
	expected := "Pulling image " + source + "\n"
	firstLine := make([]byte, len(expected))
	_, firstErr := io.ReadFull(follow, firstLine)
	require.NoError(t, firstErr)
	require.Equal(t, expected, string(firstLine))
	releasePull()
	remaining, remainingErr := io.ReadAll(follow)
	require.NoError(t, remainingErr)
	require.Empty(t, remaining)
	running := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning && current.Status.StartupLogs.Complete, nil
	})
	image := &apiv2.PhysicalContainerImage{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.StartupLogs.Name}, image))
	imageOut, imageOutErr := readV2ContainerLog(ctx, image, apiv2.ResourceLogOptions{})
	require.NoError(t, imageOutErr)
	require.Equal(t, expected, imageOut)
	logicalOut, logicalOutErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "startup_stdout"})
	require.NoError(t, logicalOutErr)
	require.Equal(t, imageOut, logicalOut)
	logicalErrOut, logicalErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "startup_stderr"})
	require.NoError(t, logicalErr)
	require.Equal(t, "Resolving image manifest\n", logicalErrOut)
}

// Verifies that owned physical image failures reach both image and logical system endpoints,
// while image pull output remains readable after the logical container fails before runtime creation.
func TestV2ImageSystemLogsFollowLogicalOwnership(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-image-system-owner")
	source := namespace.Name + "-source"
	containerOrchestrator.FailNextPullImage(source, errors.New("owned-image-system-log-test"))
	noRetries := int32(0)
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: source, PullRetryLimit: &noRetries},
		}},
	}
	require.NoError(t, client.Create(ctx, container))
	failed := waitObjectAssumesState(t, ctx, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseFailed && current.Status.StartupLogs != nil, nil
	})
	require.Empty(t, failed.Status.ContainerID)
	image := &apiv2.PhysicalContainerImage{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: failed.Status.StartupLogs.Name}, image))
	imageSystem, imageSystemErr := readV2ContainerLog(ctx, image, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, imageSystemErr)
	logicalSystem, logicalSystemErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, logicalSystemErr)
	require.Contains(t, imageSystem, "owned-image-system-log-test")
	require.Contains(t, logicalSystem, "owned-image-system-log-test")
	require.Contains(t, logicalSystem, "PhysicalContainerImageReconciler")
	startup, startupErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true})
	require.NoError(t, startupErr)
	require.Equal(t, "Pulling image "+source+"\n", startup)
}

// Verifies that multiple containers referencing an explicitly supplied image share its build output
// without forwarding that image's system diagnostics into their logical system streams.
func TestV2ContainerStartupLogsExposeReferencedImageBuild(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-image-startup-build")
	image := &apiv2.PhysicalContainerImage{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-image", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{
			Image: namespace.Name + "-output",
			Build: &apiv2.ContainerBuildContext{Context: t.TempDir(), Digest: "build-v1"},
		}},
	}
	require.NoError(t, client.Create(ctx, image))
	waitPhysicalContainerImagePhase(t, ctx, image.NamespacedName(), apiv2.PhysicalContainerImagePhaseReady)
	first := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{ImageRef: image.Name}},
	}
	second := first.DeepCopy()
	second.Name = "second"
	require.NoError(t, client.Create(ctx, first))
	require.NoError(t, client.Create(ctx, second))
	waitObjectAssumesState(t, ctx, first.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	waitObjectAssumesState(t, ctx, second.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	imageOut, imageErr := readV2ContainerLog(ctx, image, apiv2.ResourceLogOptions{})
	require.NoError(t, imageErr)
	require.Equal(t, "Building image\n", imageOut)
	firstOut, firstErr := readV2ContainerLog(ctx, first, apiv2.ResourceLogOptions{Source: "startup_stdout"})
	require.NoError(t, firstErr)
	secondOut, secondErr := readV2ContainerLog(ctx, second, apiv2.ResourceLogOptions{Source: "startup_stdout"})
	require.NoError(t, secondErr)
	require.Equal(t, imageOut, firstOut)
	require.Equal(t, imageOut, secondOut)
	buildErrOut, buildErr := readV2ContainerLog(ctx, first, apiv2.ResourceLogOptions{Source: "startup_stderr"})
	require.NoError(t, buildErr)
	require.Equal(t, "Preparing build context\n", buildErrOut)
	firstSystem, firstSystemErr := readV2ContainerLog(ctx, first, apiv2.ResourceLogOptions{Source: "system"})
	require.NoError(t, firstSystemErr)
	require.NotContains(t, firstSystem, "Building PhysicalContainerImage")
}

// Verifies that a logical-only stdout follower finishes when its physical backing disappears,
// without a physical log request starting the physical-kind lifecycle watch.
func TestV2LogicalContainerLogFollowerEndsWhenPhysicalBackingIsDeleted(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startErr := StartTestEnvironment(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController,
		t.Name(), t.TempDir())
	require.NoError(t, startErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "logical-only-log-follow"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	runtimeID, runErr := orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{Name: "logical-only-runtime", Image: "existing-image"},
	})
	require.NoError(t, runErr)
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerSpec{Mode: apiv2.LogicalContainerModeExisting, ContainerName: "logical-only-runtime"},
	}
	require.NoError(t, server.Client.Create(ctx, container))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.NoError(t, orchestrator.SimulateContainerLogging(runtimeID, apiv1.LogStreamSourceStdout, []byte("history\n")))
	// EOF is the behavior under test; bound a regression independently of environment shutdown.
	streamCtx, cancelStream := context.WithTimeout(ctx, 15*time.Second)
	defer cancelStream()
	gvr := container.GetGroupVersionResource()
	follower, followErr := server.RestClient.Get().
		AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", namespace.Name, gvr.Resource, container.Name, apiv2.LogSubresourceName).
		SpecificallyVersionedParams(&apiv2.ResourceLogOptions{Follow: true},
			runtime.NewParameterCodec(dcpclient.NewScheme()), apiv2.GroupVersion).Stream(streamCtx)
	require.NoError(t, followErr)
	defer func() { require.NoError(t, follower.Close()) }()
	history := make([]byte, len("history\n"))
	_, historyErr := io.ReadFull(follower, history)
	require.NoError(t, historyErr)
	require.Equal(t, "history\n", string(history))
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
		Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef,
	}, physical))
	require.NoError(t, server.Client.Delete(ctx, physical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	waitObjectAssumesStateEx(t, ctx, server.Client, container.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.ContainerID == "" && current.Status.Phase == apiv2.LogicalContainerPhaseUnknown, nil
	})
	remaining, readErr := io.ReadAll(follower)
	require.NoError(t, readErr)
	require.Empty(t, remaining)
	missing := &apiv2.LogicalContainer{}
	require.NoError(t, server.Client.Get(ctx, container.NamespacedName(), missing))
	require.NotNil(t, missing.Status.RuntimeLogs)
	require.Equal(t, physical.UID, missing.Status.RuntimeLogs.UID)
	require.True(t, missing.Status.RuntimeLogs.Complete)
	require.False(t, missing.Status.RuntimeLogs.Available)
	require.NoError(t, server.Client.Delete(ctx, container))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, container)
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
	noStartup, noStartupErr := readV2ContainerLog(ctx, container, apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true})
	require.NoError(t, noStartupErr)
	require.Empty(t, noStartup)
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
