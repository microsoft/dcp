/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that confirmed runtime container loss fails both container layers and new executions,
// without launching commands or recreating the missing container.
func TestV2ContainerExecRejectsConfirmedMissingRuntime(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-missing-runtime")
	_, removeErr := containerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{target.Status.ContainerID}, Force: true,
	})
	require.NoError(t, removeErr)
	physicalTarget := types.NamespacedName{Namespace: target.Namespace, Name: target.Status.PhysicalContainerRef}
	missing := waitPhysicalContainerPhase(t, ctx, physicalTarget, apiv2.PhysicalContainerPhaseFailed)
	requireReadyCondition(t, missing.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerMissing)
	waitObjectAssumesState(t, ctx, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
	})
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name,
			Execution: apiv2.ContainerExecConfig{Command: "missing-runtime-logical"}}}
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: physicalTarget.Name,
			Execution: apiv2.ContainerExecConfig{Command: "missing-runtime-physical"}}}
	require.NoError(t, client.Create(ctx, logical))
	require.NoError(t, client.Create(ctx, physical))
	failedLogical := waitV2LogicalExecPhase(t, ctx, logical, apiv2.ContainerExecPhaseFailed)
	failedPhysical := waitObjectAssumesState(t, ctx, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	requireReadyCondition(t, failedLogical.Status.Conditions, metav1.ConditionFalse, apiv2.ContainerExecReasonContainerUnavailable)
	requireReadyCondition(t, failedPhysical.Status.Conditions, metav1.ConditionFalse, apiv2.ContainerExecReasonContainerUnavailable)
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(logical.Spec.Execution.Command))
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(physical.Spec.Execution.Command))
	_, inspectErr := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{target.Status.ContainerID}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

// Verifies that physical exec rejects confirmed runtime loss between target selection and
// uncached preflight, reporting the terminal target state without a misleading nil lookup error.
func TestV2ContainerExecRejectsRuntimeLossDuringPreflight(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var reader *containerExecLaunchReader
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
			reader = &containerExecLaunchReader{Reader: apiReader, entered: make(chan struct{}), release: make(chan struct{})}
			return reader
		}})
	require.NoError(t, startupErr)
	unblock := sync.OnceFunc(func() { close(reader.release) })
	t.Cleanup(unblock)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-preflight-runtime-loss")
	reader.target = types.NamespacedName{Namespace: target.Namespace, Name: target.Status.PhysicalContainerRef}
	reader.enabled.Store(true)
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: reader.target.Name,
			Execution: apiv2.ContainerExecConfig{Command: "preflight-runtime-loss"}}}
	require.NoError(t, server.Client.Create(ctx, physical))
	waitContainerExecSignal(t, ctx, reader.entered)
	orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, simulated)
	_, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{Containers: []string{target.Status.ContainerID}, Force: true})
	require.NoError(t, removeErr)
	waitObjectAssumesStateEx(t, ctx, server.Client, reader.target, func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhaseFailed, nil
	})
	unblock()
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ContainerExecReasonContainerUnavailable)
	condition := apimeta.FindStatusCondition(failed.Status.Conditions, string(apiv2.ConditionReady))
	require.Equal(t, "target PhysicalContainer has exited or failed before execution launch", condition.Message)
	require.NotContains(t, condition.Message, "<nil>")
	require.Equal(t, target.Status.ContainerID, failed.Status.ContainerID)
	require.Zero(t, orchestrator.ExecContainerCallCount(physical.Spec.Execution.Command))
}

// Verifies that unrecognized runtime observations remain retryable Unknown container states,
// leaving executions pending until the same container becomes running again.
func TestV2ContainerExecWaitsForTransientUnknownTarget(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	target := createV2ExecTarget(t, ctx, client, "v2-exec-unknown-target")
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, target.Status.ContainerID, containers.ContainerStatus("unrecognized")))
	waitPhysicalContainerPhase(t, ctx, types.NamespacedName{Namespace: target.Namespace, Name: target.Status.PhysicalContainerRef},
		apiv2.PhysicalContainerPhaseUnknown)
	waitObjectAssumesState(t, ctx, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseUnknown, nil
	})
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "unknown-target-logical"}}}
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef,
			Execution: apiv2.ContainerExecConfig{Command: "unknown-target-physical"}}}
	require.NoError(t, client.Create(ctx, logical))
	require.NoError(t, client.Create(ctx, physical))
	waitObjectAssumesState(t, ctx, logical.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return len(current.Status.Conditions) != 0 && current.Status.Conditions[0].Reason == string(apiv2.ContainerExecReasonContainerNotReady), nil
	})
	waitObjectAssumesState(t, ctx, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return len(current.Status.Conditions) != 0 && current.Status.Conditions[0].Reason == string(apiv2.ContainerExecReasonContainerNotReady), nil
	})
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(logical.Spec.Execution.Command))
	require.Zero(t, containerOrchestrator.ExecContainerCallCount(physical.Spec.Execution.Command))
	require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, target.Status.ContainerID, containers.ContainerStatusRunning))
	waitV2LogicalExecPhase(t, ctx, logical, apiv2.ContainerExecPhaseRunning)
	waitObjectAssumesState(t, ctx, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
}
