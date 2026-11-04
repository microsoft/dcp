/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// Verifies that persistent logical process leases do not share rows with containers or
// networks even when their namespace and name would otherwise form the same resource key.
func TestV2LogicalProcessLeaseKeysDoNotCollideWithRuntimeResources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	for _, namespace := range []string{"containers", "containernetworks"} {
		logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process-lease-cross-kind", Namespace: namespace}}
		var other statestore.LeasableResource = &apiv2.LogicalContainer{Spec: apiv2.LogicalContainerSpec{ContainerName: logical.Name}}
		if namespace == "containernetworks" {
			other = &apiv2.LogicalContainerNetwork{Spec: apiv2.LogicalContainerNetworkSpec{NetworkName: logical.Name}}
		}
		processLease, processLeaseErr := testStateStore.AcquireResourceLease(ctx, logical, testResourceLeaseOwner, time.Second)
		require.NoError(t, processLeaseErr)
		defer func() { require.NoError(t, processLease.Release(context.WithoutCancel(ctx))) }()
		otherLease, otherLeaseErr := testStateStore.AcquireResourceLease(ctx, other, testResourceLeaseOwner, time.Second)
		require.NoError(t, otherLeaseErr)
		require.NoError(t, otherLease.Release(ctx))
		require.NoError(t, testStateStore.VerifyResourceLeaseHeld(ctx, logical, testResourceLeaseOwner))
	}
}

type processCreateLostResponseClient struct {
	ctrl_client.Client
	created       chan *apiv2.PhysicalProcess
	release       <-chan struct{}
	responseError error
	injected      atomic.Bool
}

// Verifies that forcibly removing the backing PhysicalProcess before startup settlement terminally fails the
// logical process, releases its lease, and permits namespace deletion without recreating or adopting a process.
func TestV2LogicalProcessControllerFailsWhenPersistentBackingDisappears(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"before-observation", "during-recording"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespaceName := "v2-process-missing-" + stage
			created, release := make(chan *apiv2.PhysicalProcess, 1), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			var reader *namespaceWorkloadFailureReader
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						return &processCreateLostResponseClient{Client: apiClient, created: created, release: release}
					},
					DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
						reader = &namespaceWorkloadFailureReader{Reader: apiReader, namespaceName: namespaceName}
						return reader
					},
				})
			require.NoError(t, startupErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName,
				Annotations: map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "missing-process-workload"}}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			reader.fail.Store(stage == "during-recording")
			defer reader.fail.Store(false)
			command := "v2-process-missing-" + stage
			logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespaceName},
				Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
					Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
			require.NoError(t, server.Client.Create(ctx, logical))
			var physical *apiv2.PhysicalProcess
			select {
			case physical = <-created:
			case <-ctx.Done():
				t.Fatal("physical process was not created")
			}
			require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
			require.Contains(t, physical.Finalizers, apiv2.GroupName+"/physicalprocess-reconciler")
			running := waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, nil
			})
			pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
			require.NoError(t, pidErr)
			handle := process.NewHandle(pid, running.Status.IdentityTimestamp.Time)
			defer func() {
				require.NoError(t, environment.TestProcessExecutor.StopProcess(context.WithoutCancel(ctx), handle))
			}()
			if stage == "during-recording" {
				unblock()
				waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
					ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return ready != nil && ready.Reason == "PersistenceFailed", nil
				})
			}
			require.NoError(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner))
			forceRemoveLogicalProcessBacking(t, ctx, server.Client, physical)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			unblock()
			failed := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.LogicalProcessPhaseFailed &&
					ready != nil && ready.Reason == "PhysicalProcessMissing", nil
			})
			require.NotNil(t, failed.Status.RuntimeLogs)
			require.Equal(t, physical.UID, failed.Status.RuntimeLogs.UID)
			require.True(t, failed.Status.RuntimeLogs.Complete)
			require.False(t, failed.Status.RuntimeLogs.Available)
			require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
			_, recordErr := environment.StateStore.GetPersistentProcess(ctx, logical.GetLeaseKey())
			require.ErrorIs(t, recordErr, statestore.ErrPersistentProcessNotFound)
			require.NoError(t, environment.TestProcessExecutor.CheckProcessRunning(handle))
			require.NoError(t, retryOnConflictEx(ctx, server.Client, logical.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
				current.Spec.Stop = true
				return server.Client.Update(updateCtx, current)
			}))
			waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Spec.Stop && current.Status.Phase == apiv2.LogicalProcessPhaseFailed &&
					ready != nil && ready.ObservedGeneration == current.Generation, nil
			})
			require.True(t, apierrors.IsNotFound(server.Client.Get(ctx, physical.NamespacedName(), &apiv2.PhysicalProcess{})))
			require.Len(t, environment.TestProcessExecutor.FindAll([]string{command}, "", nil), 1)
			require.NoError(t, server.Client.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
		})
	}
}

func (boundary *processCreateLostResponseClient) Create(
	ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption,
) error {
	createErr := boundary.Client.Create(ctx, object, options...)
	physical, isPhysicalProcess := object.(*apiv2.PhysicalProcess)
	if createErr != nil || !isPhysicalProcess || !boundary.injected.CompareAndSwap(false, true) {
		return createErr
	}
	boundary.created <- physical.DeepCopy()
	select {
	case <-boundary.release:
		return boundary.responseError
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Verifies that LogicalProcess recovers an owned child after a lost successful create response,
// forwards a newer stop request, and preserves the original execution and log identity without relaunching.
func TestV2LogicalProcessControllerRecoversCreatedChildBeforeForwardingStop(t *testing.T) {
	t.Parallel()
	for _, mode := range []apiv2.LogicalProcessMode{apiv2.LogicalProcessModeSession, apiv2.LogicalProcessModePersistent} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			created, release := make(chan *apiv2.PhysicalProcess, 1), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					return &processCreateLostResponseClient{Client: apiClient, created: created, release: release,
						responseError: errors.New("applied physical process create response lost")}
				}})
			require.NoError(t, startupErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-process-lost-create-" + string(mode)}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			command := "v2-process-lost-create-command-" + string(mode)
			logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
				Spec: apiv2.LogicalProcessSpec{Mode: mode, Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
			require.NoError(t, server.Client.Create(ctx, logical))
			var physical *apiv2.PhysicalProcess
			select {
			case physical = <-created:
			case <-ctx.Done():
				t.Fatal("physical creation did not complete")
			}
			require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
			require.Contains(t, physical.Finalizers, apiv2.GroupName+"/physicalprocess-reconciler")
			running := waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, nil
			})
			require.NoError(t, server.Client.Patch(ctx, logical,
				ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			unblock()
			stopped := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
			})
			require.Equal(t, physical.Name, stopped.Status.PhysicalProcessRef)
			require.Equal(t, running.Status.PID, stopped.Status.PID)
			require.NotNil(t, stopped.Status.RuntimeLogs)
			require.Equal(t, physical.UID, stopped.Status.RuntimeLogs.UID)
			require.True(t, stopped.Status.RuntimeLogs.Complete)
			currentPhysical := &apiv2.PhysicalProcess{}
			require.NoError(t, server.Client.Get(ctx, physical.NamespacedName(), currentPhysical))
			require.Equal(t, physical.UID, currentPhysical.UID)
			require.True(t, currentPhysical.Spec.Stop)
			require.Len(t, environment.TestProcessExecutor.FindAll([]string{command}, "", nil), 1)
			require.NoError(t, server.Client.Delete(ctx, logical))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
		})
	}
}

// Verifies that LogicalProcess waits for namespace readiness, delegates session launch and configuration,
// and stops the process by deleting its backing PhysicalProcess.
func TestV2LogicalProcessControllerDelegatesSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "v2-logical-process-session"},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{
			ExecutablePath: "v2-logical-process-session-command", Args: []string{"one"}, WorkingDirectory: "/tmp",
			AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
			Env:                []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}},
		}}}
	require.NoError(t, client.Create(ctx, resource))
	pending := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == string(apiv2.PhysicalResourceReasonNamespaceNotFound), nil
	})
	require.Empty(t, pending.Status.PhysicalProcessRef)
	createActiveV2Namespace(t, ctx, resource.Namespace)
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: running.Status.PhysicalProcessRef}, physical))
	require.True(t, metav1.IsControlledBy(physical, resource))
	require.False(t, physical.Spec.Process.RetainRuntimeProcess)
	require.Equal(t, resource.Spec.Process.AmbientEnvironment, physical.Spec.Process.AmbientEnvironment)
	executions := testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil)
	require.Len(t, executions, 1)
	require.Equal(t, []string{"v2-logical-process-session-command", "one"}, executions[0].Cmd.Args)
	require.Equal(t, []string{"VALUE=expected"}, executions[0].Cmd.Env)
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	handle := process.NewHandle(pid, running.Status.IdentityTimestamp.Time)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(handle)))
}

// Verifies that LogicalProcess deletion before namespace admission does not prepare lifecycle policy,
// acquire a persistent lease, or create a backing PhysicalProcess.
func TestV2LogicalProcessControllerDeletesBeforeNamespaceAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "v2-process-delete-before-admission"},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-delete-before-admission-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	pending := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.PhysicalResourceReasonNamespaceNotFound), nil
	})
	require.Empty(t, pending.Status.LifecycleKey)
	require.Empty(t, pending.Status.PhysicalProcessRef)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, resource, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	_, recordErr := testStateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.ErrorIs(t, recordErr, statestore.ErrPersistentProcessNotFound)
	var children apiv2.PhysicalProcessList
	require.NoError(t, client.List(ctx, &children, ctrl_client.InNamespace(resource.Namespace)))
	require.Empty(t, children.Items)
}

type processBackingLookupFailureClient struct {
	ctrl_client.Client
	namespaceName string
	fail          atomic.Bool
}

func (reader *processBackingLookupFailureClient) List(
	ctx context.Context, object ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) error {
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if _, physical := object.(*apiv2.PhysicalProcessList); physical && listOptions.Namespace == reader.namespaceName && reader.fail.Load() {
		return errors.New("physical process lookup unavailable")
	}
	return reader.Client.List(ctx, object, options...)
}

// Verifies that LogicalProcess preserves process identity during a recoverable backing lookup failure,
// then forwards stop and observes completion after the reader recovers without launching another process.
func TestV2LogicalProcessControllerRetriesBackingObservation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespaceName = "v2-process-observation-retry"
	var reader *processBackingLookupFailureClient
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			reader = &processBackingLookupFailureClient{Client: apiClient, namespaceName: namespaceName}
			return reader
		}})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	const command = "v2-process-observation-retry-command"
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespaceName},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	reader.fail.Store(true)
	defer reader.fail.Store(false)
	require.NoError(t, server.Client.Patch(ctx, resource,
		ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	unavailable := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.ObservedGeneration == current.Generation &&
			ready.Reason == "PhysicalProcessLookupFailed", nil
	})
	require.Equal(t, apiv2.LogicalProcessPhaseUnknown, unavailable.Status.Phase)
	require.Equal(t, running.Status.PID, unavailable.Status.PID)
	require.Equal(t, running.Status.PhysicalProcessRef, unavailable.Status.PhysicalProcessRef)
	require.Equal(t, running.Status.RuntimeLogs, unavailable.Status.RuntimeLogs)
	reader.fail.Store(false)
	stopped := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
	})
	require.Equal(t, running.Status.PID, stopped.Status.PID)
	require.Len(t, environment.TestProcessExecutor.FindAll([]string{command}, "", nil), 1)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that losing the logical backing hold terminally fails LogicalProcess without forwarding
// a later stop request, while deleting the logical resource still cleans up its owned session process.
func TestV2LogicalProcessControllerRejectsReleasedBackingHold(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-released-backing-hold")
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-released-hold-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	physicalName := types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalProcessRef}
	require.NoError(t, retryOnConflict(ctx, physicalName, func(updateCtx context.Context, current *apiv2.PhysicalProcess) error {
		patch := ctrl_client.MergeFromWithOptions(current.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool {
			return finalizer == controllers.LogicalBackingFinalizer
		})
		return client.Patch(updateCtx, current, patch)
	}))
	failed := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalProcessPhaseFailed && ready != nil && ready.Reason == "PhysicalProcessChanged", nil
	})
	require.True(t, failed.Status.RuntimeLogs.Complete)
	require.False(t, failed.Status.RuntimeLogs.Available)
	require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.ObservedGeneration == current.Generation &&
			ready.Reason == "PhysicalProcessChanged", nil
	})
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, physicalName, physical))
	require.False(t, physical.Spec.Stop)
	pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	handle := process.NewHandle(pid, running.Status.IdentityTimestamp.Time)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
	require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(handle)))
}

// Verifies that LogicalProcess preserves workload ownership and capture paths through unscoped persistent reuse.
// Changed lifecycle keys replace the process without inheriting the retired process's workload association.
func TestV2LogicalProcessControllerReusesAndReplacesPersistentProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-persistent")
	require.NoError(t, retryOnConflict(ctx, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		current.Annotations = map[string]string{apiv2.NamespaceWorkloadIDAnnotation: " logical-process-workload "}
		return client.Update(updateCtx, current)
	}))
	spec := apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
		Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-process-persistent-command"}}
	first := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, first))
	created := waitLogicalProcessPhase(t, ctx, first.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	record, recordErr := testStateStore.GetPersistentProcess(ctx, first.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, commonapi.WorkloadID("logical-process-workload"), record.WorkloadID)
	require.Equal(t, *created.Status.PID, int64(record.PID))
	require.Equal(t, created.Status.LifecycleKey, record.LifecycleKey)
	require.Equal(t, created.Status.RuntimeLogs.StdoutFile, record.StdOutFile)
	require.Equal(t, created.Status.RuntimeLogs.StderrFile, record.StdErrFile)
	stdout, stdoutErr := usvc_io.OpenOrCreateFileForAppending(record.StdOutFile, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdoutErr)
	_, writeErr := io.WriteString(stdout, "123 retained output\n")
	require.NoError(t, writeErr)
	require.NoError(t, stdout.Close())
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, first, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	require.NoError(t, client.Delete(ctx, first))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(record.ProcessHandle()))
	require.FileExists(t, record.StdOutFile)
	require.FileExists(t, record.StdErrFile)
	require.NoError(t, retryOnConflict(ctx, namespace.NamespacedName(), func(updateCtx context.Context, current *apiv2.Namespace) error {
		delete(current.Annotations, apiv2.NamespaceWorkloadIDAnnotation)
		return client.Update(updateCtx, current)
	}))
	second := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, second))
	reused := waitLogicalProcessPhase(t, ctx, second.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.Equal(t, *created.Status.PID, *reused.Status.PID)
	adopted := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: reused.Status.PhysicalProcessRef}, adopted))
	require.Equal(t, reused.Status.PID, adopted.Spec.PID)
	require.False(t, adopted.Spec.IdentityTimestamp.IsZero())
	require.Nil(t, adopted.Spec.Process)
	require.Equal(t, record.StdOutFile, adopted.Spec.StdoutFile)
	require.Equal(t, record.StdErrFile, adopted.Spec.StderrFile)
	require.Equal(t, record.StdOutFile, reused.Status.RuntimeLogs.StdoutFile)
	output, outputErr := readV2ContainerLog(ctx, second, apiv2.ResourceLogOptions{})
	require.NoError(t, outputErr)
	require.Equal(t, "123 retained output\n", output)
	reusedRecord, reusedRecordErr := testStateStore.GetPersistentProcess(ctx, second.GetLeaseKey())
	require.NoError(t, reusedRecordErr)
	require.Equal(t, record.RunID, reusedRecord.RunID)
	require.Equal(t, record.WorkloadID, reusedRecord.WorkloadID)
	require.Equal(t, record.StdOutFile, reusedRecord.StdOutFile)
	require.Equal(t, record.StdErrFile, reusedRecord.StdErrFile)
	owned, ownedErr := testStateStore.ListPersistentProcessesByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, ownedErr)
	require.Len(t, owned, 1)
	require.Equal(t, record.PID, owned[0].PID)
	require.NoError(t, client.Delete(ctx, second))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, second)
	spec.Process.Args = []string{"changed"}
	third := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, third))
	replaced := waitLogicalProcessPhase(t, ctx, third.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, *created.Status.PID, *replaced.Status.PID)
	replacedRecord, replacedRecordErr := testStateStore.GetPersistentProcess(ctx, third.GetLeaseKey())
	require.NoError(t, replacedRecordErr)
	require.Empty(t, replacedRecord.WorkloadID)
	retiredOwnerRecords, retiredOwnerErr := testStateStore.ListPersistentProcessesByWorkloadID(ctx, record.WorkloadID)
	require.NoError(t, retiredOwnerErr)
	require.Empty(t, retiredOwnerRecords)
	require.NoFileExists(t, record.StdOutFile)
	require.NoFileExists(t, record.StdErrFile)
	require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(record.ProcessHandle())))
	previous := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(third.UID) + "-previous"}, previous))
	require.True(t, previous.Spec.Stop)
	require.Equal(t, *created.Status.PID, *previous.Spec.PID)
	require.Equal(t, apiv2.PhysicalProcessPhaseExited, previous.Status.Phase)
	require.NoError(t, retryOnConflict(ctx, third.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	waitLogicalProcessPhase(t, ctx, third.NamespacedName(), apiv2.LogicalProcessPhaseExited)
	require.NoError(t, client.Delete(ctx, third))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, third)
	require.NoFileExists(t, replaced.Status.RuntimeLogs.StdoutFile)
	require.NoFileExists(t, replaced.Status.RuntimeLogs.StderrFile)
}

// Verifies that LogicalProcess existing mode binds the supplied identity, never launches a process,
// retains it on deletion, and rejects stale PID identities without stopping the current process.
func TestV2LogicalProcessControllerExistingIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-existing")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-logical-process-existing-command")
	pid := int64(handle.Pid)
	existing := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModeExisting, PID: &pid, IdentityTimestamp: metav1.NewMicroTime(handle.IdentityTime)}}
	require.NoError(t, client.Create(ctx, existing))
	waitLogicalProcessPhase(t, ctx, existing.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NoError(t, client.Delete(ctx, existing))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, existing)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	stale := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "stale", Namespace: namespace.Name}, Spec: *existing.Spec.DeepCopy()}
	stale.Spec.IdentityTimestamp = metav1.NewMicroTime(handle.IdentityTime.Add(-time.Hour))
	stale.Spec.Stop = true
	require.NoError(t, client.Create(ctx, stale))
	waitLogicalProcessPhase(t, ctx, stale.NamespacedName(), apiv2.LogicalProcessPhaseExited)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	require.NoError(t, client.Delete(ctx, stale))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, stale)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	require.Len(t, testProcessExecutor.FindAll([]string{"v2-logical-process-existing-command"}, "", nil), 1)
}

// Verifies that LogicalProcess does not launch a process when created with stop requested.
func TestV2LogicalProcessControllerDoesNotLaunchStoppedProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-stopped")
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "stopped", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Stop: true, Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-process-stopped-command"}}}
	require.NoError(t, client.Create(ctx, resource))
	waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseExited)
	require.Empty(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil))
}

// Verifies that LogicalProcess discards a persistent record whose PID identity is stale,
// creates a new process, and does not stop the unrelated process currently using that PID.
func TestV2LogicalProcessControllerRejectsReusedPersistentPID(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-stale-record")
	currentHandle := seedAdoptablePhysicalProcess(t, ctx, "v2-logical-process-reused-pid-command")
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-process-new-pid-command"}}}
	key, _, keyErr := resource.Spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	stdoutPath, stderrPath := filepath.Join(t.TempDir(), "old-out"), filepath.Join(t.TempDir(), "old-err")
	require.NoError(t, usvc_io.WriteFile(stdoutPath, []byte("stale output"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, usvc_io.WriteFile(stderrPath, []byte("stale errors"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, testStateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey: resource.GetLeaseKey(), LifecycleKey: key, PID: currentHandle.Pid,
		IdentityTime: currentHandle.IdentityTime.Add(-time.Hour), RunID: "stale-run",
		StdOutFile: stdoutPath, StdErrFile: stderrPath,
	}))
	require.NoError(t, client.Create(ctx, resource))
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, int64(currentHandle.Pid), *running.Status.PID)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(currentHandle))
	stored, storedErr := testStateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.NoError(t, storedErr)
	require.Equal(t, *running.Status.PID, int64(stored.PID))
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)
}

// Verifies that replacement retains the old process record through retired-output cleanup
// failure and launches the new process only after cleanup and record retirement succeed.
func TestV2LogicalProcessControllerRetriesRetiredOutputCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-process-retired-output-retry")
	first := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-retired-output-command"}}}
	require.NoError(t, client.Create(ctx, first))
	waitLogicalProcessPhase(t, ctx, first.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	record, recordErr := testStateStore.GetPersistentProcess(ctx, first.GetLeaseKey())
	require.NoError(t, recordErr)
	require.NoError(t, client.Delete(ctx, first))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
	saved := record.StdOutFile + ".saved"
	obstruction := filepath.Join(record.StdOutFile, "busy")
	t.Cleanup(func() {
		_ = os.Remove(obstruction)
		_ = os.Remove(record.StdOutFile)
		_ = os.Remove(saved)
	})
	require.NoError(t, os.Rename(record.StdOutFile, saved))
	require.NoError(t, os.Mkdir(record.StdOutFile, 0700))
	require.NoError(t, usvc_io.WriteFile(obstruction, []byte("busy"), osutil.PermissionOnlyOwnerReadWrite))
	replacement := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: first.Name, Namespace: namespace.Name}, Spec: *first.Spec.DeepCopy()}
	replacement.Spec.Process.Args = []string{"replacement"}
	require.NoError(t, client.Create(ctx, replacement))
	waitObjectAssumesState(t, ctx, replacement.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return condition != nil && condition.Reason == "PhysicalProcessPreparationFailed", nil
	})
	preserved, preservedErr := testStateStore.GetPersistentProcess(ctx, first.GetLeaseKey())
	require.NoError(t, preservedErr)
	require.Equal(t, record.PID, preserved.PID)
	require.Equal(t, record.StdOutFile, preserved.StdOutFile)
	require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(record.ProcessHandle())))
	require.Len(t, testProcessExecutor.FindAll([]string{first.Spec.Process.ExecutablePath}, "", nil), 1)
	require.NoError(t, os.Remove(obstruction))
	replaced := waitLogicalProcessPhase(t, ctx, replacement.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, int64(record.PID), *replaced.Status.PID)
	newRecord, newRecordErr := testStateStore.GetPersistentProcess(ctx, replacement.GetLeaseKey())
	require.NoError(t, newRecordErr)
	require.Equal(t, replaced.Status.RuntimeLogs.StdoutFile, newRecord.StdOutFile)
	require.NoDirExists(t, record.StdOutFile)
	require.NoFileExists(t, record.StdErrFile)
	require.NoError(t, retryOnConflict(ctx, replacement.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
		current.Spec.Stop = true
		return client.Update(updateCtx, current)
	}))
	waitLogicalProcessPhase(t, ctx, replacement.NamespacedName(), apiv2.LogicalProcessPhaseExited)
	require.NoError(t, client.Delete(ctx, replacement))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, replacement)
}

func waitLogicalProcessPhase(t *testing.T, ctx context.Context, name types.NamespacedName, phase apiv2.LogicalProcessPhase) *apiv2.LogicalProcess {
	t.Helper()
	return waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == phase, nil
	})
}

type processBackingReleaseResponseClient struct {
	ctrl_client.Client
	namespaceName string
	injected      atomic.Bool
}

func (apiClient *processBackingReleaseResponseClient) Patch(
	ctx context.Context, object ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption,
) error {
	patchErr := apiClient.Client.Patch(ctx, object, patch, options...)
	if _, physical := object.(*apiv2.PhysicalProcess); patchErr == nil && physical &&
		object.GetNamespace() == apiClient.namespaceName && !object.GetDeletionTimestamp().IsZero() &&
		!slices.Contains(object.GetFinalizers(), controllers.LogicalBackingFinalizer) &&
		apiClient.injected.CompareAndSwap(false, true) {
		return errors.New("logical process backing release response lost")
	}
	return patchErr
}

// Verifies that LogicalProcess permits PhysicalProcess cleanup while ownership recording fails, retains its
// startup lease and backing hold, then completes registration and hold release despite a lost release response.
func TestV2LogicalProcessControllerStopsDuringPersistenceFailureAndSettlesBeforeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespaceName = "v2-logical-process-ownership"
	var reader *namespaceWorkloadFailureReader
	var releaseClient *processBackingReleaseResponseClient
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
				reader = &namespaceWorkloadFailureReader{Reader: apiReader, namespaceName: namespaceName}
				return reader
			},
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				releaseClient = &processBackingReleaseResponseClient{Client: apiClient, namespaceName: namespaceName}
				return releaseClient
			},
		})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName,
		Annotations: map[string]string{apiv2.NamespaceWorkloadIDAnnotation: "retained-process-workload"}}}
	require.NoError(t, serverInfo.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	reader.fail.Store(true)
	defer reader.fail.Store(false)
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespaceName},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-process-ownership-command"}}}
	require.NoError(t, serverInfo.Client.Create(ctx, resource))
	failed := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "PersistenceFailed" && current.Status.PID != nil, nil
	})
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, resource, environmentInfo.ResourceLeaseOwner))
	_, recordErr := environmentInfo.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.ErrorIs(t, recordErr, statestore.ErrPersistentProcessNotFound)
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalProcess) error {
		current.Spec.Stop = true
		return serverInfo.Client.Update(updateCtx, current)
	}))
	physicalName := types.NamespacedName{Namespace: namespaceName, Name: failed.Status.PhysicalProcessRef}
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, physicalName, func(current *apiv2.PhysicalProcess) (bool, error) {
		return current.Spec.Stop && current.Status.Phase == apiv2.PhysicalProcessPhaseExited, nil
	})
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, resource, environmentInfo.ResourceLeaseOwner))
	require.NoError(t, serverInfo.Client.Delete(ctx, resource))
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return !current.DeletionTimestamp.IsZero(), nil
	})
	failuresBeforeRetry := reader.failures.Load()
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return reader.failures.Load() > failuresBeforeRetry, nil
	}))
	physical := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, physicalName, func(current *apiv2.PhysicalProcess) (bool, error) {
		return !current.DeletionTimestamp.IsZero() &&
			!slices.Contains(current.Finalizers, apiv2.GroupName+"/physicalprocess-reconciler"), nil
	})
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.NoError(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, resource, environmentInfo.ResourceLeaseOwner))
	reader.fail.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, physical)
	require.True(t, releaseClient.injected.Load())
	record, storedErr := environmentInfo.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.NoError(t, storedErr)
	require.Equal(t, commonapi.WorkloadID("retained-process-workload"), record.WorkloadID)
	require.Equal(t, *failed.Status.PID, int64(record.PID))
	require.True(t, process.IsProcessGoneErr(environmentInfo.TestProcessExecutor.CheckProcessRunning(record.ProcessHandle())))
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, resource, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

// Verifies that a confirmed missing adopted process permits deletion and startup lease release
// without consulting unavailable workload ownership or removing its existing reuse record.
func TestV2LogicalProcessControllerReleasesLeaseForMissingAdoptedProcess(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespaceName = "v2-process-missing-adoption-registration"
	var reader *namespaceWorkloadFailureReader
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
			reader = &namespaceWorkloadFailureReader{Reader: apiReader, namespaceName: namespaceName}
			return reader
		}})
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespaceName},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-process-missing-adoption-command"}}}
	handle, _, startErr := environment.TestProcessExecutor.StartProcess(ctx, exec.Command(resource.Spec.Process.ExecutablePath),
		nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	key, _, keyErr := resource.Spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.NoError(t, environment.StateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey: resource.GetLeaseKey(), LifecycleKey: key, PID: handle.Pid, IdentityTime: handle.IdentityTime, RunID: "original-run",
	}))
	reader.fail.Store(true)
	defer reader.fail.Store(false)
	require.NoError(t, server.Client.Create(ctx, resource))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "PersistenceFailed", nil
	})
	require.NoError(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, resource, environment.ResourceLeaseOwner))
	require.NoError(t, environment.TestProcessExecutor.StopProcess(ctx, handle))
	physicalName := types.NamespacedName{Namespace: namespaceName, Name: failed.Status.PhysicalProcessRef}
	waitObjectAssumesStateEx(t, ctx, server.Client, physicalName, func(current *apiv2.PhysicalProcess) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.PhysicalProcessReasonRuntimeProcessMissing), nil
	})
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, resource, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	record, recordErr := environment.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.NoError(t, recordErr)
	require.Equal(t, "original-run", record.RunID)
	require.Empty(t, record.WorkloadID)
}

// Verifies that LogicalProcess namespace cleanup removes LogicalProcess and PhysicalProcess resources,
// stops session processes, and preserves persistent processes.
func TestV2LogicalProcessControllerNamespaceCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-namespace")
	handles := make(map[apiv2.LogicalProcessMode]process.ProcessHandle)
	for _, mode := range []apiv2.LogicalProcessMode{apiv2.LogicalProcessModeSession, apiv2.LogicalProcessModePersistent} {
		resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: string(mode), Namespace: namespace.Name},
			Spec: apiv2.LogicalProcessSpec{Mode: mode,
				Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-logical-process-namespace-" + string(mode)}}}
		require.NoError(t, client.Create(ctx, resource))
		running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
		pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
		require.NoError(t, pidErr)
		handles[mode] = process.NewHandle(pid, running.Status.IdentityTimestamp.Time)
	}
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
	require.True(t, process.IsProcessGoneErr(testProcessExecutor.CheckProcessRunning(handles[apiv2.LogicalProcessModeSession])))
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handles[apiv2.LogicalProcessModePersistent]))
}

type retainedProcessLaunchGate struct {
	process.Executor
	command string
	started chan<- struct{}
	release <-chan struct{}
}

func (gate *retainedProcessLaunchGate) StartProcess(
	ctx context.Context, command *exec.Cmd, handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag, create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	if command.Path == gate.command {
		close(gate.started)
		select {
		case <-gate.release:
		case <-ctx.Done():
			return process.ProcessHandle{}, nil, ctx.Err()
		}
	}
	return gate.Executor.StartProcess(ctx, command, handler, flags, create)
}

// Verifies that LogicalProcess requests backing deletion during an in-flight retained launch,
// preserves its identity for ownership recording, and does not block unrelated process launches.
func TestV2LogicalProcessControllerDeletesDuringRetainedLaunch(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"logical", "namespace"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			command := "v2-logical-process-delete-launch-" + target
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecoratePhysicalProcessExecutor: func(executor process.Executor) process.Executor {
					return &retainedProcessLaunchGate{Executor: executor, command: command, started: started, release: release}
				}})
			require.NoError(t, startupErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-logical-process-delete-launch-" + target}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
				Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
					Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("physical process launch did not start")
			}
			unrelated := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
				Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "v2-unrelated-process-" + target}}}
			require.NoError(t, server.Client.Create(ctx, unrelated))
			waitObjectAssumesStateEx(t, ctx, server.Client, unrelated.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
				return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
			})
			physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID), Namespace: namespace.Name}}
			if target == "logical" {
				require.NoError(t, server.Client.Delete(ctx, resource))
			} else {
				require.NoError(t, server.Client.Delete(ctx, namespace))
			}
			waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(),
				func(current *apiv2.LogicalProcess) (bool, error) { return !current.DeletionTimestamp.IsZero(), nil })
			waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			require.NoError(t, server.Client.Get(ctx, physical.NamespacedName(), physical))
			require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
			require.Contains(t, physical.Finalizers, apiv2.GroupName+"/physicalprocess-reconciler")
			unblock()
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			record, recordErr := environment.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
			require.NoError(t, recordErr)
			require.NoError(t, environment.TestProcessExecutor.CheckProcessRunning(record.ProcessHandle()))
			require.Len(t, environment.TestProcessExecutor.FindAll([]string{command}, "", nil), 1)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			if target == "logical" {
				require.NoError(t, server.Client.Delete(ctx, namespace))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, unrelated)
		})
	}
}

func forceRemoveLogicalProcessBacking(
	t *testing.T, ctx context.Context, apiClient ctrl_client.Client, physical *apiv2.PhysicalProcess,
) {
	t.Helper()
	require.NoError(t, apiClient.Delete(ctx, physical))
	waitObjectAssumesStateEx(t, ctx, apiClient, physical.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
		return !slices.Contains(current.Finalizers, apiv2.GroupName+"/physicalprocess-reconciler"), nil
	})
	// Deliberately violate the logical hold to exercise terminal backing-loss handling.
	require.NoError(t, retryOnConflictEx(ctx, apiClient, physical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalProcess) error {
		patch := ctrl_client.MergeFromWithOptions(current.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
		current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool {
			return finalizer == controllers.LogicalBackingFinalizer
		})
		return apiClient.Patch(updateCtx, current, patch)
	}))
}

// Verifies that LogicalProcess projects terminal PhysicalProcess launch failure and releases the
// persistent startup lease even when no process was successfully launched.
func TestV2LogicalProcessControllerDeletesAfterLaunchFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-delete-failure")
	command := "v2-logical-process-delete-failure-command"
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{command}}
	testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{Condition: criteria,
		StartupError: func(*internal_testutil.ProcessExecution) error { return errors.New("launch unavailable") }})
	defer testProcessExecutor.RemoveAutoExecution(criteria)
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
	require.NoError(t, client.Create(ctx, resource))
	waitObjectAssumesState(t, ctx, types.NamespacedName{Namespace: namespace.Name, Name: "logical-" + string(resource.UID)},
		func(current *apiv2.PhysicalProcess) (bool, error) {
			ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
			return ready != nil && ready.Reason == string(apiv2.PhysicalProcessReasonLaunchFailed), nil
		})
	waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseFailed)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, resource, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	_, recordErr := testStateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.ErrorIs(t, recordErr, statestore.ErrPersistentProcessNotFound)
}
