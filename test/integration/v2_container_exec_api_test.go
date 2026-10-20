/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcpclient"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	containerExecTestControllers = LogicalContainerController | PhysicalContainerController |
		PhysicalContainerImageController | PhysicalContainerExecController | LogicalContainerExecController
	containerExecReconcileStep = "exec-test-reconcile"
)

type containerExecReconcileObserver struct {
	ctrl_client.Client
	first      chan struct{}
	second     chan struct{}
	firstOnce  sync.Once
	secondOnce sync.Once
}

func (observer *containerExecReconcileObserver) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	getErr := observer.Client.Get(ctx, key, obj, options...)
	if execution, physical := obj.(*apiv2.PhysicalContainerExec); physical && getErr == nil {
		switch execution.Annotations[containerExecReconcileStep] {
		case "first":
			observer.firstOnce.Do(func() { close(observer.first) })
		case "second":
			observer.secondOnce.Do(func() { close(observer.second) })
		}
	}
	return getErr
}

type containerExecStatusConflictClient struct {
	ctrl_client.Client
	conflicted atomic.Bool
}

func (apiClient *containerExecStatusConflictClient) Status() ctrl_client.SubResourceWriter {
	return &containerExecStatusConflictWriter{SubResourceWriter: apiClient.Client.Status(), client: apiClient}
}

type containerExecStatusConflictWriter struct {
	ctrl_client.SubResourceWriter
	client *containerExecStatusConflictClient
}

func (writer *containerExecStatusConflictWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	if execution, logical := obj.(*apiv2.LogicalContainerExec); logical &&
		execution.Status.Phase == apiv2.ContainerExecPhaseFailed && execution.Status.RuntimeLogs != nil &&
		execution.Status.RuntimeLogs.Complete && !execution.Status.RuntimeLogs.Available &&
		writer.client.conflicted.CompareAndSwap(false, true) {
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "logicalcontainerexecs"},
			execution.Name, errors.New("injected status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

type containerExecLaunchReader struct {
	ctrl_client.Reader
	target  types.NamespacedName
	enabled atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (reader *containerExecLaunchReader) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && reader.enabled.Load() && key == reader.target {
		reader.once.Do(func() {
			close(reader.entered)
			select {
			case <-reader.release:
			case <-ctx.Done():
			}
		})
	}
	return reader.Reader.Get(ctx, key, obj, options...)
}

func waitContainerExecSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("container execution did not reach the expected API operation")
	}
}

// Verifies that LogicalContainerExec repeats terminal source invalidation after a status conflict,
// preserving completion and the original physical identity without retaining deleted capture paths.
func TestV2ContainerExecMissingBackingProjectionSurvivesStatusConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var conflictClient *containerExecStatusConflictClient
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			conflictClient = &containerExecStatusConflictClient{Client: apiClient}
			return conflictClient
		}})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	target := createV2ExecTarget(t, ctx, apiClient, "v2-exec-status-conflict")
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-status-conflict-command"}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	running := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalExecRef, Namespace: target.Namespace}}
	require.NoError(t, apiClient.Delete(ctx, physical))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerExec](t, ctx, apiClient, physical)
	failed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	require.True(t, conflictClient.conflicted.Load())
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "PhysicalExecutionChanged")
	require.Equal(t, &apiv2.LogSource{Name: running.Status.PhysicalExecRef, UID: running.Status.RuntimeLogs.UID, Complete: true}, failed.Status.RuntimeLogs)
}

// Verifies that PhysicalContainerExec waits when its target is paused during uncached preflight,
// then starts exactly once when that same runtime container becomes ready again.
func TestV2PhysicalContainerExecRetriesReadinessLossBeforeLaunch(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var launchReader *containerExecLaunchReader
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
			launchReader = &containerExecLaunchReader{Reader: reader, entered: make(chan struct{}), release: make(chan struct{})}
			return launchReader
		}})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	target := createV2ExecTarget(t, ctx, apiClient, "v2-exec-preflight-pause")
	launchReader.target = types.NamespacedName{Namespace: target.Namespace, Name: target.Status.PhysicalContainerRef}
	launchReader.enabled.Store(true)
	resource := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-preflight-pause-command"}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	waitContainerExecSignal(t, ctx, launchReader.entered)
	orchestrator, isSimulated := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isSimulated)
	require.NoError(t, orchestrator.SimulateContainerStatus(ctx, target.Status.ContainerID, containers.ContainerStatusPaused))
	waitObjectAssumesStateEx(t, ctx, apiClient, launchReader.target, func(current *apiv2.PhysicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.PhysicalContainerPhasePaused, nil
	})
	close(launchReader.release)
	waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.ContainerExecPhasePending && condition != nil && condition.Reason == "ContainerNotReady", nil
	})
	require.Zero(t, orchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
	require.NoError(t, orchestrator.SimulateContainerStatus(ctx, target.Status.ContainerID, containers.ContainerStatusRunning))
	running := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	require.Equal(t, target.Status.ContainerID, running.Status.ContainerID)
	require.Equal(t, 1, orchestrator.ExecContainerCallCount(resource.Spec.Execution.Command))
	require.NoError(t, orchestrator.SimulateContainerExecExit(ctx, target.Status.ContainerID, resource.Spec.Execution.Command, 0))
}

// Verifies that logical-only stdout followers finish after physical backing deletion,
// without opening a physical log endpoint or waiting for request cancellation.
func TestV2ContainerExecLogicalOnlyFollowerCompletesOnBackingDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	serverInfo, _, startupErr := StartTestEnvironment(t, ctx, containerExecTestControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	target := createV2ExecTarget(t, ctx, apiClient, "v2-exec-logical-only-follow")
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-logical-only-follow-command"}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	running := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	orchestrator, isSimulated := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isSimulated)
	require.NoError(t, orchestrator.SimulateContainerExecCommandLogging(running.Status.ContainerID, resource.Spec.Execution.Command,
		apiv1.LogStreamSourceStdout, []byte("initial-output\n")))
	gvr := resource.GetGroupVersionResource()
	options := apiv2.ResourceLogOptions{Follow: true}
	follow, followErr := serverInfo.RestClient.Get().
		AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", resource.Namespace, gvr.Resource, resource.Name, apiv2.LogSubresourceName).
		SpecificallyVersionedParams(&options, runtime.NewParameterCodec(dcpclient.NewScheme()), apiv2.GroupVersion).Stream(ctx)
	require.NoError(t, followErr)
	defer func() { require.NoError(t, follow.Close()) }()
	initial := make([]byte, len("initial-output\n"))
	_, initialErr := io.ReadFull(follow, initial)
	require.NoError(t, initialErr)
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalExecRef, Namespace: resource.Namespace}}
	require.NoError(t, apiClient.Delete(ctx, physical))
	ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainerExec](t, ctx, apiClient, physical)
	failed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseFailed, nil
	})
	require.False(t, failed.Status.RuntimeLogs.Available)
	require.True(t, failed.Status.RuntimeLogs.Complete)
	remaining, completionErr := io.ReadAll(follow)
	require.NoError(t, completionErr)
	require.Empty(t, remaining)
}
