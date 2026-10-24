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
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
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

// Verifies that losing retained physical backing before startup settlement terminally fails the
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
					DecorateClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
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
			require.NoError(t, server.Client.Delete(ctx, physical))
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
				TestEnvironmentOptions{DecorateClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
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
// and stops the runtime process by deleting its physical backing.
func TestV2LogicalProcessControllerDelegatesSessionLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	inherit := false
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "v2-logical-process-session"},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{
			ExecutablePath: "v2-logical-process-session-command", Args: []string{"one"}, WorkingDirectory: "/tmp",
			InheritEnvironment: &inherit, Env: []commonapi.EnvVar{{Name: "VALUE", Value: "expected"}},
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

// Verifies that LogicalProcess records namespace ownership, retains and reuses matching persistent processes,
// and delegates changed-lifecycle replacement to an identity-bound stopping physical resource.
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
	require.ErrorIs(t, testStateStore.VerifyResourceLeaseHeld(ctx, first, testResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
	require.NoError(t, client.Delete(ctx, first))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, first)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(record.ProcessHandle()))
	second := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, second))
	reused := waitLogicalProcessPhase(t, ctx, second.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.Equal(t, *created.Status.PID, *reused.Status.PID)
	adopted := &apiv2.PhysicalProcess{}
	require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: reused.Status.PhysicalProcessRef}, adopted))
	require.Equal(t, reused.Status.PID, adopted.Spec.PID)
	require.False(t, adopted.Spec.IdentityTimestamp.IsZero())
	require.Nil(t, adopted.Spec.Process)
	reusedRecord, reusedRecordErr := testStateStore.GetPersistentProcess(ctx, second.GetLeaseKey())
	require.NoError(t, reusedRecordErr)
	require.Equal(t, record.RunID, reusedRecord.RunID)
	require.NoError(t, client.Delete(ctx, second))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, second)
	spec.Process.Args = []string{"changed"}
	third := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name}, Spec: *spec.DeepCopy()}
	require.NoError(t, client.Create(ctx, third))
	replaced := waitLogicalProcessPhase(t, ctx, third.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, *created.Status.PID, *replaced.Status.PID)
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

// Verifies that LogicalProcess suppresses launch when stop is requested before physical creation.
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
	require.NoError(t, testStateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey: resource.GetLeaseKey(), LifecycleKey: key, PID: currentHandle.Pid,
		IdentityTime: currentHandle.IdentityTime.Add(-time.Hour), RunID: "stale-run",
	}))
	require.NoError(t, client.Create(ctx, resource))
	running := waitLogicalProcessPhase(t, ctx, resource.NamespacedName(), apiv2.LogicalProcessPhaseRunning)
	require.NotEqual(t, int64(currentHandle.Pid), *running.Status.PID)
	require.NoError(t, testProcessExecutor.CheckProcessRunning(currentHandle))
	stored, storedErr := testStateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.NoError(t, storedErr)
	require.Equal(t, *running.Status.PID, int64(stored.PID))
}

func waitLogicalProcessPhase(t *testing.T, ctx context.Context, name types.NamespacedName, phase apiv2.LogicalProcessPhase) *apiv2.LogicalProcess {
	t.Helper()
	return waitObjectAssumesState(t, ctx, name, func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == phase, nil
	})
}

// Verifies that LogicalProcess forwards stop while ownership recording fails, retains its startup
// lease and deletion finalizer, then records ownership before releasing the lease and deleting its backing.
func TestV2LogicalProcessControllerStopsDuringPersistenceFailureAndSettlesBeforeDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	const namespaceName = "v2-logical-process-ownership"
	var reader *namespaceWorkloadFailureReader
	serverInfo, environmentInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
			reader = &namespaceWorkloadFailureReader{Reader: apiReader, namespaceName: namespaceName}
			return reader
		}})
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
	physical := &apiv2.PhysicalProcess{}
	require.NoError(t, serverInfo.Client.Get(ctx, types.NamespacedName{Namespace: namespaceName, Name: failed.Status.PhysicalProcessRef}, physical))
	require.True(t, physical.DeletionTimestamp.IsZero(), "physical deletion must wait for ownership recording")
	require.Len(t, physical.Finalizers, 1, "only the physical controller owns a finalizer on its backing resource")
	reader.fail.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, resource)
	record, storedErr := environmentInfo.StateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
	require.NoError(t, storedErr)
	require.Equal(t, commonapi.WorkloadID("retained-process-workload"), record.WorkloadID)
	require.Equal(t, *failed.Status.PID, int64(record.PID))
	require.True(t, process.IsProcessGoneErr(environmentInfo.TestProcessExecutor.CheckProcessRunning(record.ProcessHandle())))
	require.ErrorIs(t, environmentInfo.StateStore.VerifyResourceLeaseHeld(ctx, resource, environmentInfo.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
}

// Verifies that LogicalProcess namespace cleanup removes logical and physical resources,
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

// Verifies that LogicalProcess settles an in-flight retained launch and records its identity
// before deleting physical backing during logical-resource or namespace deletion.
func TestV2LogicalProcessControllerDeletesDuringRetainedLaunch(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"logical", "namespace"} {
		t.Run(target, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-logical-process-delete-launch-"+target)
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			command := "v2-logical-process-delete-launch-" + target
			criteria := internal_testutil.ProcessSearchCriteria{Command: []string{command}}
			testProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{Condition: criteria,
				StartupError: func(*internal_testutil.ProcessExecution) error {
					close(started)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}})
			defer func() {
				unblock()
				testProcessExecutor.RemoveAutoExecution(criteria)
			}()
			resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
				Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
					Process: &apiv2.LogicalProcessConfig{ExecutablePath: command}}}
			require.NoError(t, client.Create(ctx, resource))
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("physical process launch did not start")
			}
			physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "logical-" + string(resource.UID), Namespace: namespace.Name}}
			if target == "logical" {
				require.NoError(t, client.Delete(ctx, resource))
			} else {
				require.NoError(t, client.Delete(ctx, namespace))
			}
			waitObjectAssumesState(t, ctx, resource.NamespacedName(),
				func(current *apiv2.LogicalProcess) (bool, error) { return !current.DeletionTimestamp.IsZero(), nil })
			require.NoError(t, client.Get(ctx, physical.NamespacedName(), physical))
			require.True(t, physical.DeletionTimestamp.IsZero(), "physical deletion must wait for startup settlement")
			require.Len(t, physical.Finalizers, 1)
			unblock()
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
			record, recordErr := testStateStore.GetPersistentProcess(ctx, resource.GetLeaseKey())
			require.NoError(t, recordErr)
			require.NoError(t, testProcessExecutor.CheckProcessRunning(record.ProcessHandle()))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
			if target == "namespace" {
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
			}
		})
	}
}

// Verifies that LogicalProcess projects terminal physical launch failure and releases the
// persistent startup lease even when no runtime process was successfully launched.
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
