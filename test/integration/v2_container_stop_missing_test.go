/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"sync"
	"sync/atomic"
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

type containerStopObservationClient struct {
	ctrl_client.Client
	observed chan struct{}
	release  <-chan struct{}
	paused   atomic.Bool
}

func (apiClient *containerStopObservationClient) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	getErr := apiClient.Client.Get(ctx, key, object, options...)
	if getErr != nil {
		return getErr
	}
	return apiClient.pauseAfterStop(ctx, object)
}

func (apiClient *containerStopObservationClient) pauseAfterStop(ctx context.Context, object ctrl_client.Object) error {
	physical, container := object.(*apiv2.PhysicalContainer)
	if !container || !physical.Spec.Stop || !physical.DeletionTimestamp.IsZero() || !apiClient.paused.CompareAndSwap(false, true) {
		return nil
	}
	close(apiClient.observed)
	select {
	case <-apiClient.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type containerStopObservationReader struct {
	ctrl_client.Reader
	observation *containerStopObservationClient
}

func (reader *containerStopObservationReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	getErr := reader.Reader.Get(ctx, key, object, options...)
	if getErr != nil {
		return getErr
	}
	return reader.observation.pauseAfterStop(ctx, object)
}

// Verifies that nonterminal PhysicalContainer observations treat runtime absence as successful
// completion when Stop is already requested, allowing the logical container to finish stopping.
func TestV2ContainerStopCompletesWhenRuntimeDisappears(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		status containers.ContainerStatus
		phase  apiv2.PhysicalContainerPhase
		reason apiv2.ConditionReason
	}{
		{"running", containers.ContainerStatusRunning, apiv2.PhysicalContainerPhaseRunning, apiv2.PhysicalContainerReasonRuntimeContainerRunning},
		{"starting", containers.ContainerStatusRestarting, apiv2.PhysicalContainerPhasePending, apiv2.PhysicalContainerReasonRuntimeContainerRestarting},
		{"unknown", containers.ContainerStatus("unrecognized"), apiv2.PhysicalContainerPhaseUnknown, apiv2.PhysicalContainerReasonRuntimeContainerStatusUnknown},
		{"removing", containers.ContainerStatusRemoving, apiv2.PhysicalContainerPhasePending, apiv2.PhysicalContainerReasonRuntimeContainerRemoving},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			release := make(chan struct{})
			var observation *containerStopObservationClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						observation = &containerStopObservationClient{Client: apiClient, observed: make(chan struct{}), release: release}
						return observation
					},
					DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
						return &containerStopObservationReader{Reader: apiReader, observation: observation}
					},
				})
			require.NoError(t, startupErr)
			apiClient := server.Client
			target := createV2ExecTarget(t, ctx, apiClient, "v2-stop-missing")
			resume := sync.OnceFunc(func() { close(release) })
			t.Cleanup(resume)
			orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.True(t, simulated)
			physicalName := types.NamespacedName{Name: target.Status.PhysicalContainerRef, Namespace: target.Namespace}
			require.NoError(t, orchestrator.SimulateContainerStatus(ctx, target.Status.ContainerID, testCase.status))
			waitObjectAssumesStateEx(t, ctx, apiClient, physicalName, func(current *apiv2.PhysicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == testCase.phase && ready != nil && ready.Reason == string(testCase.reason), nil
			})
			physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: physicalName.Name, Namespace: physicalName.Namespace}}
			require.NoError(t, apiClient.Patch(ctx, physical, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			require.NoError(t, apiClient.Patch(ctx, target, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			select {
			case <-observation.observed:
			case <-ctx.Done():
				t.Fatal("physical controller did not observe Stop")
			}
			waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return ready != nil && ready.ObservedGeneration == current.Generation && current.Spec.Stop, nil
			})
			require.NoError(t, orchestrator.SimulateContainerStatus(ctx, target.Status.ContainerID, containers.ContainerStatusExited))
			_, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
				Containers: []string{target.Status.ContainerID}, Force: true,
			})
			require.NoError(t, removeErr)
			resume()
			stopped := waitObjectAssumesStateEx(t, ctx, apiClient, physicalName, func(current *apiv2.PhysicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalContainerPhaseExited, nil
			})
			requireReadyCondition(t, stopped.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonStopRequested)
			logical := waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
			})
			requireReadyCondition(t, logical.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerReasonStopped)
		})
	}
}

// Verifies that runtime absence observed before Stop remains a terminal physical and logical
// failure after later stop requests, without rewriting the missing-runtime diagnostic.
func TestV2ContainerStopPreservesEstablishedMissingFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{})
	require.NoError(t, startupErr)
	apiClient := server.Client
	target := createV2ExecTarget(t, ctx, apiClient, "v2-stop-after-missing")
	physicalName := types.NamespacedName{Name: target.Status.PhysicalContainerRef, Namespace: target.Namespace}
	_, removeErr := server.ContainerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{target.Status.ContainerID}, Force: true,
	})
	require.NoError(t, removeErr)
	physical := waitObjectAssumesStateEx(t, ctx, apiClient, physicalName, func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhaseFailed, nil
	})
	requireReadyCondition(t, physical.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerMissing)
	waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
	})
	require.NoError(t, apiClient.Patch(ctx, physical, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	unchanged := waitObjectAssumesStateEx(t, ctx, apiClient, physicalName, func(current *apiv2.PhysicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Spec.Stop && ready != nil && ready.ObservedGeneration == current.Generation, nil
	})
	require.Equal(t, apiv2.PhysicalContainerPhaseFailed, unchanged.Status.Phase)
	requireReadyCondition(t, unchanged.Status.Conditions, metav1.ConditionFalse, apiv2.PhysicalContainerReasonRuntimeContainerMissing)
	require.NoError(t, apiClient.Patch(ctx, target, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	logical := waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Spec.Stop && ready != nil && ready.ObservedGeneration == current.Generation, nil
	})
	require.Equal(t, apiv2.LogicalContainerPhaseFailed, logical.Status.Phase)
	requireReadyCondition(t, logical.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerReasonMissing)
}
