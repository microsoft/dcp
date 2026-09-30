/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

type serverProxyStartTestExecutor struct {
	process.Executor

	handle            process.ProcessHandle
	startCalls        int
	stopCalls         int
	stopErrors        []error
	exitHandlers      []process.ProcessExitHandler
	exitDuringStop    bool
	stopNotifications chan process.ProcessHandle
	outputFiles       []string
	outputFileHandles []*os.File
}

func (executor *serverProxyStartTestExecutor) StartProcess(
	_ context.Context,
	cmd *exec.Cmd,
	handler process.ProcessExitHandler,
	_ process.ProcessCreationFlag,
	_ process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.startCalls++
	executor.exitHandlers = append(executor.exitHandlers, handler)
	for _, output := range []any{cmd.Stdout, cmd.Stderr} {
		if file, isFile := output.(*os.File); isFile {
			executor.outputFiles = append(executor.outputFiles, file.Name())
			executor.outputFileHandles = append(executor.outputFileHandles, file)
		}
	}
	return executor.handle, func() {}, nil
}

func (executor *serverProxyStartTestExecutor) StopProcess(
	_ context.Context,
	handle process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	stopIndex := executor.stopCalls
	executor.stopCalls++
	if executor.exitDuringStop && stopIndex < len(executor.exitHandlers) && executor.exitHandlers[stopIndex] != nil {
		executor.exitHandlers[stopIndex].OnProcessExited(handle.Pid, 0, nil)
	}
	if stopIndex < len(executor.stopErrors) {
		return executor.stopErrors[stopIndex]
	}
	if executor.stopNotifications != nil {
		executor.stopNotifications <- handle
	}
	return nil
}

func (executor *serverProxyStartTestExecutor) StartAndForget(
	_ *exec.Cmd,
	_ process.ProcessCreationFlag,
) (process.ProcessHandle, error) {
	return executor.handle, nil
}

// Verifies that a server proxy configuration failure preserves process identity after cleanup fails,
// reports both failures, and allows a later cleanup attempt to clear the identity.
func TestServerProxyConfigFailurePreservesIdentityAfterCleanupFailure(t *testing.T) {
	dcppaths.EnableTestPathProbing()

	configReadErr := errors.New("server proxy configuration is unavailable")
	stopErr := errors.New("server proxy stop failed")
	executor := &serverProxyStartTestExecutor{
		handle:     process.NewHandle(4200, time.Unix(1000, 0).UTC()),
		stopErrors: []error{stopErr, nil},
	}
	t.Cleanup(func() {
		for _, outputFile := range executor.outputFiles {
			_ = os.Remove(outputFile)
		}
	})
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{}, configReadErr
			},
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "config-failure",
			UID:  types.UID("config-failure-uid"),
		},
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)

	_, started := reconciler.startServerProxy(context.Background(), proxy, proxyData, logr.Discard())

	require.False(t, started)
	require.Equal(t, 1, executor.startCalls)
	require.Equal(t, 1, executor.stopCalls)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, proxyData.State)
	require.NotNil(t, proxyData.ServerProxyProcessID)
	require.Equal(t, int64(executor.handle.Pid), *proxyData.ServerProxyProcessID)
	require.Equal(t, executor.handle.IdentityTime, proxyData.ServerProxyStartupTimestamp.Time)
	require.Contains(t, proxyData.Message, configReadErr.Error())
	require.Contains(t, proxyData.Message, stopErr.Error())

	retryStopErr := reconciler.stopServerProxyProcess(context.Background(), proxyData)
	require.NoError(t, retryStopErr)
	require.Equal(t, 1, executor.startCalls, "cleanup retry must not launch another server proxy")
	require.Equal(t, 2, executor.stopCalls)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.True(t, proxyData.ServerProxyStartupTimestamp.IsZero())
}

// Verifies that confirmed cleanup after a server proxy configuration failure clears process identity,
// keeps the proxy retryable, and does not start another process immediately.
func TestServerProxyConfigFailureClearsIdentityAfterConfirmedCleanup(t *testing.T) {
	dcppaths.EnableTestPathProbing()

	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4201, time.Unix(1001, 0).UTC()),
	}
	t.Cleanup(func() {
		for _, outputFile := range executor.outputFiles {
			_ = os.Remove(outputFile)
		}
	})
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{}, errors.New("server proxy configuration is unavailable")
			},
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, "confirmed-cleanup-uid")

	_, started := reconciler.startServerProxy(
		context.Background(),
		&apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name: "confirmed-cleanup",
				UID:  types.UID("confirmed-cleanup-uid"),
			},
		},
		proxyData,
		logr.Discard(),
	)

	require.False(t, started)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateStarting, proxyData.State)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.True(t, proxyData.ServerProxyStartupTimestamp.IsZero())
	require.Equal(t, 1, executor.startCalls)
	require.Equal(t, 1, executor.stopCalls)
}

// Verifies that expected rollback exits do not fail startup, stale callbacks cannot affect a replacement,
// and the current server proxy callback still records an unexpected exit.
func TestServerProxyExitCallbacksAreRunScoped(t *testing.T) {
	dcppaths.EnableTestPathProbing()

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()
	executor := &serverProxyStartTestExecutor{
		handle:         process.NewHandle(4202, time.Unix(1002, 0).UTC()),
		exitDuringStop: true,
	}
	t.Cleanup(func() {
		for _, outputFile := range executor.outputFiles {
			_ = os.Remove(outputFile)
		}
	})
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "callback-during-rollback",
			UID:  types.UID("callback-during-rollback-uid"),
		},
	}
	proxyName := proxy.NamespacedName()
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](
			nil,
			nil,
			logr.Discard(),
			testCtx,
		),
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{}, errors.New("server proxy configuration is unavailable")
			},
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	reconciler.proxyData.Store(proxyName, proxyName, proxyData.Clone())

	run, started := reconciler.startServerProxy(testCtx, proxy, proxyData, logr.Discard())
	require.False(t, started)
	require.NotNil(t, run)
	recordedExitType, exited := run.getExitType()
	require.True(t, exited)
	require.Equal(t, serverProxyExitTypeExpected, recordedExitType)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateStarting, proxyData.State)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.Equal(t, 1, executor.stopCalls)

	_ = reconciler.proxyData.Update(proxyName, proxyName, proxyData)
	reconciler.proxyData.RunDeferredOps(proxyName, proxy)
	_, currentData := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.NotNil(t, currentData)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateStarting, currentData.State)

	nextData := currentData.Clone()
	nextData.State = apiv1.ContainerNetworkTunnelProxyStateRunning
	nextHandle := process.NewHandle(executor.handle.Pid, time.Unix(1003, 0).UTC())
	nextPID := int64(nextHandle.Pid)
	nextData.ServerProxyProcessID = &nextPID
	nextData.ServerProxyStartupTimestamp = metav1.NewMicroTime(nextHandle.IdentityTime)
	require.True(t, reconciler.proxyData.Update(proxyName, proxyName, nextData))

	executor.exitHandlers[0].OnProcessExited(executor.handle.Pid, 0, nil)
	reconciler.proxyData.RunDeferredOps(proxyName, proxy)
	_, afterStaleExit := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, afterStaleExit.State)

	nextRun := newServerProxyRun()
	nextRun.handle = nextHandle
	reconciler.onServerProcessExit(
		proxyName,
		nextRun,
		nextHandle.Pid,
		1,
		errors.New("unexpected exit"),
		executor.outputFileHandles[0],
		executor.outputFileHandles[1],
	)
	reconciler.proxyData.RunDeferredOps(proxyName, proxy)
	_, afterCurrentExit := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, afterCurrentExit.State)
	require.Nil(t, afterCurrentExit.ServerProxyProcessID)
}

// Verifies that an unexpected server proxy exit before startup-result publication remains failed,
// clears process identity, and cannot be overwritten by the successful startup result.
func TestServerProxyExitBeforeStartupResultPublicationRemainsFailed(t *testing.T) {
	dcppaths.EnableTestPathProbing()

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()
	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4204, time.Unix(1004, 0).UTC()),
	}
	t.Cleanup(func() {
		for _, outputFile := range executor.outputFiles {
			_ = os.Remove(outputFile)
		}
	})
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exit-before-startup-publication",
			UID:  types.UID("exit-before-startup-publication-uid"),
		},
	}
	proxyName := proxy.NamespacedName()
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](
			nil,
			nil,
			logr.Discard(),
			testCtx,
		),
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{ServerControlPort: 4205}, nil
			},
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	reconciler.proxyData.Store(proxyName, proxyName, proxyData.Clone())

	run, started := reconciler.startServerProxy(testCtx, proxy, proxyData, logr.Discard())
	require.True(t, started)
	require.NotNil(t, run)
	require.Equal(t, 1, executor.startCalls)
	_, storedBeforeResult := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.NotNil(t, storedBeforeResult)
	require.Nil(t, storedBeforeResult.ServerProxyProcessID)
	require.True(t, storedBeforeResult.ServerProxyStartupTimestamp.IsZero())

	proxyData.State = apiv1.ContainerNetworkTunnelProxyStateRunning
	executor.exitHandlers[0].OnProcessExited(executor.handle.Pid, 1, errors.New("unexpected exit"))
	recordedExitType, exited := run.getExitType()
	require.True(t, exited)
	require.Equal(t, serverProxyExitTypeUnexpected, recordedExitType)
	reconciler.queueProxyPairStartupResult(proxyName, proxy.UID, proxyData, run, func() {}, logr.Discard())

	reconciler.proxyData.RunDeferredOps(proxyName, proxy)
	_, currentData := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.NotNil(t, currentData)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, currentData.State)
	require.Nil(t, currentData.ServerProxyProcessID)
	require.True(t, currentData.ServerProxyStartupTimestamp.IsZero())
}

// Verifies that startup ownership remains held until the queued startup result is published.
func TestProxyStartupLeaseReleasedAfterResultPublication(t *testing.T) {
	t.Parallel()

	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "startup-publication",
			UID:  types.UID("startup-publication-uid"),
		},
	}
	proxyName := proxy.NamespacedName()
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	reconciler.proxyData.Store(proxyName, proxyName, proxyData.Clone())

	releaseStartup, acquired := proxyData.startup.TryAcquire(t.Context())
	require.True(t, acquired)

	serverPID := int64(4200)
	result := proxyData.Clone()
	result.State = apiv1.ContainerNetworkTunnelProxyStateRunning
	result.ServerProxyProcessID = &serverPID
	reconciler.queueProxyPairStartupResult(proxyName, proxy.UID, result, nil, releaseStartup, logr.Discard())

	_, acquiredBeforePublication := proxyData.startup.TryAcquire(t.Context())
	require.False(t, acquiredBeforePublication)

	reconciler.proxyData.RunDeferredOps(proxyName, proxy)

	releaseAfterPublication, acquiredAfterPublication := proxyData.startup.TryAcquire(t.Context())
	require.True(t, acquiredAfterPublication)
	releaseAfterPublication()

	_, currentData := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.NotNil(t, currentData)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, currentData.State)
	require.NotNil(t, currentData.ServerProxyProcessID)
	require.Equal(t, serverPID, *currentData.ServerProxyProcessID)
}

func TestProxyStartupLeaseReleasedWhenResultCannotBeQueued(t *testing.T) {
	t.Parallel()

	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "missing-startup-state",
			UID:  types.UID("missing-startup-state-uid"),
		},
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}

	releaseStartup, acquired := proxyData.startup.TryAcquire(t.Context())
	require.True(t, acquired)
	reconciler.queueProxyPairStartupResult(
		proxy.NamespacedName(),
		proxy.UID,
		proxyData,
		nil,
		releaseStartup,
		logr.Discard(),
	)

	releaseAfterQueueFailure, acquiredAfterQueueFailure := proxyData.startup.TryAcquire(t.Context())
	require.True(t, acquiredAfterQueueFailure)
	releaseAfterQueueFailure()
}

// Verifies that a newly created proxy with the same name discards completed
// in-memory state owned by the previous resource UID.
func TestRecreatedProxyDropsCompletedStateFromPreviousUID(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	proxyName := types.NamespacedName{Name: "recreated", Namespace: "test"}
	oldData := newContainerNetworkTunnelProxyData(
		apiv1.ContainerNetworkTunnelProxyStateStarting,
		types.UID("old"),
	)
	oldData.cleanupScheduled = true
	oldData.cleanupCompleted = true
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](
			nil,
			nil,
			logr.Discard(),
			testCtx,
		),
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	reconciler.proxyData.Store(proxyName, proxyName, oldData)

	waiting := reconciler.resetRecreatedTunnelProxyState(
		&apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      proxyName.Name,
				Namespace: proxyName.Namespace,
				UID:       types.UID("new"),
			},
		},
		logr.Discard(),
	)

	require.False(t, waiting)
	_, remaining := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.Nil(t, remaining)
}

// Verifies that a startup result rejected by current proxy state releases its
// startup lease and stops the unpublished server proxy process.
func TestDiscardedProxyStartupResultIsCleanedUp(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	executor := &serverProxyStartTestExecutor{
		handle:            process.NewHandle(4300, time.Unix(1300, 0).UTC()),
		stopNotifications: make(chan process.ProcessHandle, 1),
	}
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "discarded",
			Namespace: "test",
			UID:       types.UID("discarded"),
		},
	}
	proxyName := proxy.NamespacedName()
	current := newContainerNetworkTunnelProxyData(
		apiv1.ContainerNetworkTunnelProxyStateStarting,
		proxy.UID,
	)
	current.cleanupCompleted = true
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](
			client,
			client,
			logr.Discard(),
			testCtx,
		),
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
		workQueue: resiliency.NewWorkQueue(testCtx, 1),
	}
	reconciler.proxyData.Store(proxyName, proxyName, current)

	result := current.Clone()
	result.cleanupCompleted = false
	serverPID := int64(executor.handle.Pid)
	result.ServerProxyProcessID = &serverPID
	result.ServerProxyStartupTimestamp = metav1.NewMicroTime(executor.handle.IdentityTime)
	releaseStartup, acquired := current.startup.TryAcquire(testCtx)
	require.True(t, acquired)

	reconciler.queueProxyPairStartupResult(
		proxyName,
		proxy.UID,
		result,
		nil,
		releaseStartup,
		logr.Discard(),
	)
	reconciler.proxyData.RunDeferredOps(proxyName, proxy)

	select {
	case stoppedHandle := <-executor.stopNotifications:
		require.Equal(t, executor.handle, stoppedHandle)
	case <-testCtx.Done():
		t.Fatal("discarded server proxy process was not stopped")
	}
	var releaseAfterCleanup func()
	acquireErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		var leaseAcquired bool
		releaseAfterCleanup, leaseAcquired = current.startup.TryAcquire(testCtx)
		return leaseAcquired, nil
	})
	require.NoError(t, acquireErr)
	releaseAfterCleanup()
}
