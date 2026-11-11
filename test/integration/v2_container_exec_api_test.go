/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"slices"
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
	"github.com/microsoft/dcp/controllers"
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
	conflicted        atomic.Bool
	replace           atomic.Bool
	observeExecutions func(*apiv2.PhysicalContainerExecList)
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
		execution.Status.Phase == apiv2.LogicalContainerExecPhaseFailed && execution.Status.RuntimeLogs != nil &&
		execution.Status.RuntimeLogs.Complete && !execution.Status.RuntimeLogs.Available &&
		writer.client.conflicted.CompareAndSwap(false, true) {
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "logicalcontainerexecs"},
			execution.Name, errors.New("injected status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

func (apiClient *containerExecStatusConflictClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	listErr := apiClient.Client.List(ctx, list, options...)
	if physicals, executions := list.(*apiv2.PhysicalContainerExecList); executions && listErr == nil && apiClient.replace.Load() {
		apiClient.observeExecutions(physicals)
	}
	return listErr
}

type containerExecReplacementReader struct {
	ctrl_client.Reader
	client *containerExecStatusConflictClient
}

func (reader *containerExecReplacementReader) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	getErr := reader.Reader.Get(ctx, key, obj, options...)
	if physical, execution := obj.(*apiv2.PhysicalContainerExec); execution && getErr == nil && reader.client.replace.Load() {
		physical.UID += "-replacement"
	}
	return getErr
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

// Verifies that LogicalContainerExec repeats terminal source invalidation through status conflicts after backing
// loss or UID replacement in either lookup path, preserving the original identity without deleted capture paths.
func TestV2ContainerExecMissingBackingProjectionSurvivesStatusConflict(t *testing.T) {
	t.Parallel()
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		var conflictClient *containerExecStatusConflictClient
		ctx, apiClient, running := startLogicalContainerExecWithLogs(t, TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				conflictClient = &containerExecStatusConflictClient{Client: apiClient}
				return conflictClient
			},
		})
		physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalExecRef, Namespace: running.Namespace}}
		forceRemoveLogicalContainerExecBacking(t, ctx, apiClient, physical)
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
		requireMissingContainerExecLogs(t, ctx, apiClient, running, conflictClient)
		deleteLogicalContainerExecAndBacking(t, ctx, apiClient, running)
	})
	t.Run("cached-replacement", func(t *testing.T) {
		t.Parallel()
		var conflictClient *containerExecStatusConflictClient
		ctx, apiClient, running := startLogicalContainerExecWithLogs(t, TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				conflictClient = &containerExecStatusConflictClient{Client: apiClient,
					observeExecutions: func(physicals *apiv2.PhysicalContainerExecList) {
						for i := range physicals.Items {
							physicals.Items[i].UID += "-replacement"
						}
					}}
				return conflictClient
			},
		})
		conflictClient.replace.Store(true)
		t.Cleanup(func() { conflictClient.replace.Store(false) })
		require.NoError(t, apiClient.Patch(ctx, running,
			ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"exec-test":"changed-uid"}}}`))))
		requireMissingContainerExecLogs(t, ctx, apiClient, running, conflictClient)
		conflictClient.replace.Store(false)
		deleteLogicalContainerExecAndBacking(t, ctx, apiClient, running)
	})
	t.Run("uncached-replacement", func(t *testing.T) {
		t.Parallel()
		var conflictClient *containerExecStatusConflictClient
		ctx, apiClient, running := startLogicalContainerExecWithLogs(t, TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				conflictClient = &containerExecStatusConflictClient{Client: apiClient,
					observeExecutions: func(physicals *apiv2.PhysicalContainerExecList) { physicals.Items = nil }}
				return conflictClient
			},
			DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
				return &containerExecReplacementReader{Reader: reader, client: conflictClient}
			},
		})
		conflictClient.replace.Store(true)
		t.Cleanup(func() { conflictClient.replace.Store(false) })
		require.NoError(t, apiClient.Patch(ctx, running,
			ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"exec-test":"changed-uid"}}}`))))
		requireMissingContainerExecLogs(t, ctx, apiClient, running, conflictClient)
		conflictClient.replace.Store(false)
		deleteLogicalContainerExecAndBacking(t, ctx, apiClient, running)
	})
}

func startLogicalContainerExecWithLogs(t *testing.T, options TestEnvironmentOptions) (context.Context, ctrl_client.Client, *apiv2.LogicalContainerExec) {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(), options)
	require.NoError(t, startupErr)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-backing-loss")
	resource := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "v2-exec-backing-loss-command"}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerExecPhaseRunning, nil
	})
	require.NotNil(t, running.Status.RuntimeLogs)
	require.True(t, running.Status.RuntimeLogs.Available)
	require.False(t, running.Status.RuntimeLogs.Complete)
	return ctx, server.Client, running
}

func requireMissingContainerExecLogs(
	t *testing.T, ctx context.Context, apiClient ctrl_client.Client, running *apiv2.LogicalContainerExec, conflictClient *containerExecStatusConflictClient,
) {
	t.Helper()
	failed := waitObjectAssumesStateEx(t, ctx, apiClient, running.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerExecPhaseFailed &&
			current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete && !current.Status.RuntimeLogs.Available, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerExecReasonMissing)
	require.Equal(t, &apiv2.LogSource{Name: running.Status.PhysicalExecRef, UID: running.Status.RuntimeLogs.UID, Complete: true}, failed.Status.RuntimeLogs)
	require.True(t, conflictClient.conflicted.Load())
}

func deleteLogicalContainerExecAndBacking(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, resource *apiv2.LogicalContainerExec) {
	t.Helper()
	require.NoError(t, apiClient.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: resource.Status.PhysicalExecRef, Namespace: resource.Namespace}}
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
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
		return current.Status.Phase == apiv2.ContainerExecPhasePending && condition != nil && condition.Reason == string(apiv2.ContainerExecReasonContainerNotReady), nil
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

// Verifies that logical-only stdout followers finish after physical backing cleanup,
// and late followers retain captured output while the logical hold keeps the backing addressable.
func TestV2ContainerExecLogicalOnlyFollowerCompletesOnBackingCleanup(t *testing.T) {
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
		return current.Status.Phase == apiv2.LogicalContainerExecPhaseRunning, nil
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
	exited := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerExecPhaseFailed &&
			current.Status.RuntimeLogs != nil && current.Status.RuntimeLogs.Complete, nil
	})
	requireReadyCondition(t, exited.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerExecReasonMissing)
	require.True(t, exited.Status.RuntimeLogs.Complete)
	require.NoError(t, apiClient.Get(ctx, physical.NamespacedName(), physical))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.NotContains(t, physical.Finalizers, apiv2.GroupName+"/physicalcontainerexec-reconciler")
	remaining, completionErr := io.ReadAll(follow)
	require.NoError(t, completionErr)
	require.Empty(t, remaining)
	lateFollow, lateFollowErr := serverInfo.RestClient.Get().
		AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", resource.Namespace, gvr.Resource, resource.Name, apiv2.LogSubresourceName).
		SpecificallyVersionedParams(&options, runtime.NewParameterCodec(dcpclient.NewScheme()), apiv2.GroupVersion).Stream(ctx)
	require.NoError(t, lateFollowErr)
	lateOutput, lateReadErr := io.ReadAll(lateFollow)
	require.NoError(t, lateFollow.Close())
	require.NoError(t, lateReadErr)
	require.Equal(t, "initial-output\n", string(lateOutput))
	require.NoError(t, apiClient.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
}

func forceRemoveLogicalContainerExecBacking(
	t *testing.T, ctx context.Context, apiClient ctrl_client.Client, physical *apiv2.PhysicalContainerExec,
) {
	t.Helper()
	require.NoError(t, apiClient.Delete(ctx, physical))
	waitObjectAssumesStateEx(t, ctx, apiClient, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return !slices.Contains(current.Finalizers, apiv2.GroupName+"/physicalcontainerexec-reconciler"), nil
	})
	// Deliberately violate the logical hold to exercise terminal backing-loss handling.
	require.NoError(t, retryOnConflictEx(ctx, apiClient, physical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainerExec) error {
		patch := ctrl_client.MergeFromWithOptions(current.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool {
			return finalizer == controllers.LogicalBackingFinalizer
		})
		return apiClient.Patch(updateCtx, current, patch)
	}))
}
