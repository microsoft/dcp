/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
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

type containerExecRetryReader struct {
	ctrl_client.Client
	enabled atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (reader *containerExecRetryReader) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, execution := obj.(*apiv2.PhysicalContainerExec); execution && reader.enabled.Load() {
		reader.once.Do(func() {
			close(reader.entered)
			select {
			case <-reader.release:
			case <-ctx.Done():
			}
		})
	}
	return reader.Client.Get(ctx, key, obj, options...)
}

type containerExecLostCreateClient struct {
	ctrl_client.Client
	responseLost atomic.Bool
	created      chan *apiv2.PhysicalContainerExec
	release      chan struct{}
}

func (apiClient *containerExecLostCreateClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := apiClient.Client.Create(ctx, obj, options...)
	if physical, execution := obj.(*apiv2.PhysicalContainerExec); execution && createErr == nil &&
		apiClient.responseLost.CompareAndSwap(false, true) {
		apiClient.created <- physical.DeepCopy()
		select {
		case <-apiClient.release:
		case <-ctx.Done():
		}
		return errors.New("physical execution create response was lost")
	}
	return createErr
}

// Verifies that PhysicalContainerExec retains its selected resource and runtime identity across
// preflight retries instead of executing against a same-name replacement.
func TestV2PhysicalContainerExecRejectsReplacementAfterPreflightRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var launchReader *containerExecLaunchReader
	var retryReader *containerExecRetryReader
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, PhysicalContainerController|PhysicalContainerExecController,
		t.Name(), t.TempDir(), TestEnvironmentOptions{
			DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
				launchReader = &containerExecLaunchReader{Reader: reader, entered: make(chan struct{}), release: make(chan struct{})}
				return launchReader
			},
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				retryReader = &containerExecRetryReader{Client: apiClient, entered: make(chan struct{}), release: make(chan struct{})}
				return retryReader
			},
		})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	namespace := createV2ExecNamespace(t, ctx, apiClient, "v2-exec-retry-replacement")
	releaseLaunch := sync.OnceFunc(func() { close(launchReader.release) })
	releaseRetry := sync.OnceFunc(func() { close(retryReader.release) })
	t.Cleanup(releaseLaunch)
	t.Cleanup(releaseRetry)
	orchestrator, isSimulated := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isSimulated)
	originalID, originalRunErr := orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{Name: namespace.Name + "-original", Image: "original-image"},
	})
	require.NoError(t, originalRunErr)
	replacementID, replacementRunErr := orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{Name: namespace.Name + "-replacement", Image: "replacement-image"},
	})
	require.NoError(t, replacementRunErr)
	target := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerSpec{ContainerID: originalID}}
	require.NoError(t, apiClient.Create(ctx, target))
	waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
	})
	launchReader.target = target.NamespacedName()
	launchReader.enabled.Store(true)
	resource := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-retry-replacement-command"}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	waitContainerExecSignal(t, ctx, launchReader.entered)
	require.NoError(t, orchestrator.SimulateContainerStatus(ctx, originalID, containers.ContainerStatusPaused))
	waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhasePaused, nil
	})
	releaseLaunch()
	selected := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.ContainerExecPhasePending && condition != nil && condition.Reason == "ContainerNotReady", nil
	})
	require.Equal(t, originalID, selected.Status.ContainerID)
	retryReader.enabled.Store(true)
	require.NoError(t, apiClient.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"exec-test":"retry"}}}`))))
	waitContainerExecSignal(t, ctx, retryReader.entered)
	require.NoError(t, apiClient.Delete(ctx, target))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, apiClient, target)
	replacement := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: target.Name, Namespace: namespace.Name},
		Spec: apiv2.PhysicalContainerSpec{ContainerID: replacementID}}
	require.NoError(t, apiClient.Create(ctx, replacement))
	runningReplacement := waitObjectAssumesStateEx(t, ctx, apiClient, replacement.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
	})
	require.NotEqual(t, target.UID, runningReplacement.UID)
	require.Equal(t, replacementID, runningReplacement.Status.ContainerID)
	releaseRetry()
	failed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "ContainerUnavailable")
	require.Equal(t, originalID, failed.Status.ContainerID)
	require.Zero(t, orchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
}

// Verifies that LogicalContainerExec recovers an applied physical creation whose response is lost,
// preserving its completed result and capture even when the logical target has already exited.
func TestV2LogicalContainerExecRecoversLostPhysicalCreateResponse(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var lostCreate *containerExecLostCreateClient
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			lostCreate = &containerExecLostCreateClient{Client: apiClient, created: make(chan *apiv2.PhysicalContainerExec, 1), release: make(chan struct{})}
			return lostCreate
		}})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	target := createV2ExecTarget(t, ctx, apiClient, "v2-exec-lost-create")
	releaseCreate := sync.OnceFunc(func() { close(lostCreate.release) })
	t.Cleanup(releaseCreate)
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-lost-create-command"}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	var physical *apiv2.PhysicalContainerExec
	select {
	case physical = <-lostCreate.created:
	case <-ctx.Done():
		t.Fatal("physical execution was not created")
	}
	waitObjectAssumesStateEx(t, ctx, apiClient, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	orchestrator, isSimulated := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isSimulated)
	require.NoError(t, orchestrator.SimulateContainerExecExit(ctx, target.Status.ContainerID, resource.Spec.Execution.Command, 23))
	completed := waitObjectAssumesStateEx(t, ctx, apiClient, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseExited, nil
	})
	require.NotNil(t, completed.Status.ExitCode)
	require.Equal(t, int32(23), *completed.Status.ExitCode)
	require.NoError(t, apiClient.Patch(ctx, target, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	waitObjectAssumesStateEx(t, ctx, apiClient, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	releaseCreate()
	exited := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseExited, nil
	})
	require.True(t, lostCreate.responseLost.Load())
	require.Equal(t, physical.Name, exited.Status.PhysicalExecRef)
	require.NotNil(t, exited.Status.ExitCode)
	require.Equal(t, int32(23), *exited.Status.ExitCode)
	require.NotNil(t, exited.Status.RuntimeLogs)
	require.Equal(t, physical.UID, exited.Status.RuntimeLogs.UID)
	require.True(t, exited.Status.RuntimeLogs.Available)
	require.True(t, exited.Status.RuntimeLogs.Complete)
	require.Equal(t, 1, orchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
}
