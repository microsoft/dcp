/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const controllerLeaseTestTimeout = 10 * time.Second

func TestPersistentContainerLeaseHeldSkipsResourceUpdate(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()
	store, storeErr := statestore.Open(ctx, statestore.Options{
		Path:        filepath.Join(t.TempDir(), "state.sqlite3"),
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, storeErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	otherOwner := leaseOwner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(-time.Hour)

	container := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api",
			Namespace: "default",
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: "api",
			Persistent:    true,
		},
	}

	_, acquireErr := store.AcquireResourceLease(ctx, container, leaseOwner, time.Minute)
	require.NoError(t, acquireErr)

	reconciler := &ContainerReconciler{
		config: ContainerReconcilerConfig{
			StateStore:         store,
			ResourceLeaseOwner: otherOwner,
		},
	}

	updateCalled := false
	leaseErr := reconciler.withPersistentContainerResourceLease(ctx, container, logr.Discard(), func(context.Context) error {
		updateCalled = true
		return nil
	})

	require.ErrorIs(t, leaseErr, statestore.ErrResourceLeaseHeld)
	require.False(t, updateCalled)
}

func TestPersistentContainerReuseReleasesResourceLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()

	store, leaseOwner, otherOwner := openContainerLeaseTestStore(t, ctx)
	containerName := "api"
	container := persistentContainerForLeaseTest(containerName)
	orchestrator := &containerLeaseTestOrchestrator{
		inspected: &containers.InspectedContainer{
			Id:        "api-container-id",
			Name:      containerName,
			Image:     container.Spec.Image,
			StartedAt: time.Now().UTC(),
			Status:    containers.ContainerStatusRunning,
		},
	}
	reconciler := newContainerReconcilerForLeaseTest(ctx, orchestrator, store, leaseOwner)

	change := handleNewContainer(ctx, reconciler, container, apiv1.ContainerStatePending, nil, logr.Discard())

	require.NotEqual(t, noChange, change)
	require.Equal(t, apiv1.ContainerStateRunning, container.Status.State)
	assertContainerLeaseReleased(t, ctx, store, container, otherOwner)
}

func TestPersistentContainerStartFalseReleasesResourceLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()

	store, leaseOwner, otherOwner := openContainerLeaseTestStore(t, ctx)
	orchestrator := &containerLeaseTestOrchestrator{}

	container := persistentContainerForLeaseTest("api")
	shouldStart := false
	container.Spec.Start = &shouldStart
	reconciler := newContainerReconcilerForLeaseTest(ctx, orchestrator, store, leaseOwner)

	change := handleNewContainer(ctx, reconciler, container, apiv1.ContainerStatePending, nil, logr.Discard())

	require.Equal(t, noChange, change)
	require.Equal(t, apiv1.ContainerStateEmpty, container.Status.State)
	assertContainerLeaseReleased(t, ctx, store, container, otherOwner)
}

func TestSetPersistentContainerStableStateReleasesLease(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		state apiv1.ContainerState
	}{
		{name: "runtime unhealthy", state: apiv1.ContainerStateRuntimeUnhealthy},
		{name: "failed to start", state: apiv1.ContainerStateFailedToStart},
		{name: "running", state: apiv1.ContainerStateRunning},
		{name: "paused", state: apiv1.ContainerStatePaused},
		{name: "exited", state: apiv1.ContainerStateExited},
		{name: "unknown", state: apiv1.ContainerStateUnknown},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
			defer cancel()

			store, leaseOwner, _ := openContainerLeaseTestStore(t, ctx)
			container := persistentContainerForLeaseTest(testCase.name)
			reconciler := newContainerReconcilerForLeaseTest(ctx, &containerLeaseTestOrchestrator{}, store, leaseOwner)

			_, acquireErr := store.AcquireResourceLease(ctx, container, leaseOwner, time.Minute)
			require.NoError(t, acquireErr)

			reconciler.setContainerState(container, testCase.state)

			verifyErr := store.VerifyResourceLeaseHeld(ctx, container, leaseOwner)
			require.ErrorIs(t, verifyErr, statestore.ErrResourceLeaseNotHeld)
		})
	}
}

func TestSetPersistentContainerTransientStateKeepsLease(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()

	store, leaseOwner, _ := openContainerLeaseTestStore(t, ctx)
	container := persistentContainerForLeaseTest("api")
	reconciler := newContainerReconcilerForLeaseTest(ctx, &containerLeaseTestOrchestrator{}, store, leaseOwner)

	_, acquireErr := store.AcquireResourceLease(ctx, container, leaseOwner, time.Minute)
	require.NoError(t, acquireErr)

	reconciler.setContainerState(container, apiv1.ContainerStateStarting)

	require.NoError(t, store.VerifyResourceLeaseHeld(ctx, container, leaseOwner))
}

func TestPersistentContainerLifecycleMonitorUsesExplicitMonitor(t *testing.T) {
	t.Parallel()

	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, controllerLeaseTestTimeout)
	defer cancel()

	processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	monitorPID := int64(12345)
	monitorTimestamp := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	rcd := newRunningContainerData(&apiv1.Container{
		Spec: apiv1.ContainerSpec{
			Persistent:       true,
			MonitorPID:       &monitorPID,
			MonitorTimestamp: monitorTimestamp,
		},
	})
	rcd.containerID = "api-container-id"
	reconciler := &ContainerReconciler{
		config: ContainerReconcilerConfig{
			ProcessExecutor: processExecutor,
		},
	}

	reconciler.runPersistentContainerLifecycleMonitor(rcd, logr.Discard())

	monitorProcesses := processExecutor.FindAll([]string{"dcp", "monitor-container"}, "", nil)
	require.Len(t, monitorProcesses, 1)
	require.Contains(t, monitorProcesses[0].Cmd.Args, "--containerID")
	require.Contains(t, monitorProcesses[0].Cmd.Args, string(rcd.containerID))
	require.Contains(t, monitorProcesses[0].Cmd.Args, "--monitor")
	require.Contains(t, monitorProcesses[0].Cmd.Args, strconv.FormatInt(monitorPID, 10))
	require.Contains(t, monitorProcesses[0].Cmd.Args, "--monitor-identity-time")
	require.Contains(t, monitorProcesses[0].Cmd.Args, monitorTimestamp.Time.Format(osutil.RFC3339MiliTimestampFormat))
}

func openContainerLeaseTestStore(t *testing.T, ctx context.Context) (*statestore.Store, process.ProcessTreeItem, process.ProcessTreeItem) {
	t.Helper()

	store, storeErr := statestore.Open(ctx, statestore.Options{
		Path:        filepath.Join(t.TempDir(), "state.sqlite3"),
		BusyTimeout: 500 * time.Millisecond,
	})
	require.NoError(t, storeErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	otherOwner := leaseOwner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(-time.Hour)

	return store, leaseOwner, otherOwner
}

func persistentContainerForLeaseTest(containerName string) *apiv1.Container {
	return &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api",
			Namespace: "default",
			UID:       "api-uid",
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: containerName,
			Image:         "api:dev",
			Persistent:    true,
		},
	}
}

func newContainerReconcilerForLeaseTest(
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	store *statestore.Store,
	leaseOwner process.ProcessTreeItem,
) *ContainerReconciler {
	lock := &sync.Mutex{}
	return &ContainerReconciler{
		ReconcilerBase:   &ReconcilerBase[apiv1.Container, *apiv1.Container]{Log: logr.Discard(), LifetimeCtx: ctx},
		ContainerWatcher: NewContainerWatcher[apiv1.Container](orchestrator, lock, ctx),
		orchestrator:     orchestrator,
		runningContainers: NewObjectStateMap[
			containerID,
			runningContainerData,
			*runningContainerData,
			*apiv1.Container,
		](),
		lock: lock,
		config: ContainerReconcilerConfig{
			StateStore:         store,
			ResourceLeaseOwner: leaseOwner,
		},
	}
}

func assertContainerLeaseReleased(
	t *testing.T,
	ctx context.Context,
	store *statestore.Store,
	container *apiv1.Container,
	owner process.ProcessTreeItem,
) {
	t.Helper()

	_, acquireErr := store.AcquireResourceLease(ctx, container, owner, time.Minute)
	require.NoError(t, acquireErr)
}

type containerLeaseTestOrchestrator struct {
	containers.ContainerOrchestrator
	inspected *containers.InspectedContainer
}

func (o *containerLeaseTestOrchestrator) CheckStatus(context.Context, containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	return containers.ContainerRuntimeStatus{
		Installed: true,
		Running:   true,
	}
}

func (o *containerLeaseTestOrchestrator) InspectContainers(context.Context, containers.InspectContainersOptions) ([]containers.InspectedContainer, error) {
	if o.inspected == nil {
		return nil, containers.ErrNotFound
	}
	return []containers.InspectedContainer{*o.inspected}, nil
}
