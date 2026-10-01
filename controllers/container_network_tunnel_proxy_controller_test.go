/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

type serverProxyStartTestExecutor struct {
	process.Executor

	handle            process.ProcessHandle
	startCalls        int
	startErrors       []error
	creationFlags     []process.ProcessCreationFlag
	watcherCalls      int
	stopCalls         int
	stopErrors        []error
	exitHandlers      []process.ProcessExitHandler
	exitDuringStop    bool
	stopNotifications chan process.ProcessHandle
	outputFileHandles []*os.File
	stopObservation   cleanupContextObservation
}

func (executor *serverProxyStartTestExecutor) StartProcess(
	_ context.Context,
	cmd *exec.Cmd,
	handler process.ProcessExitHandler,
	creationFlags process.ProcessCreationFlag,
	_ process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.startCalls++
	executor.creationFlags = append(executor.creationFlags, creationFlags)
	for _, output := range []any{cmd.Stdout, cmd.Stderr} {
		if file, isFile := output.(*os.File); isFile {
			executor.outputFileHandles = append(executor.outputFileHandles, file)
		}
	}
	startIndex := executor.startCalls - 1
	if startIndex < len(executor.startErrors) && executor.startErrors[startIndex] != nil {
		return process.ProcessHandle{}, nil, executor.startErrors[startIndex]
	}
	executor.exitHandlers = append(executor.exitHandlers, handler)
	return executor.handle, func() {}, nil
}

func (executor *serverProxyStartTestExecutor) StopProcess(
	ctx context.Context,
	handle process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	executor.stopObservation.err = ctx.Err()
	executor.stopObservation.deadline, executor.stopObservation.hasDeadline = ctx.Deadline()
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
	executor.watcherCalls++
	return executor.handle, nil
}

type tunnelProxyDeleteTestClient struct {
	ctrl_client.Client

	deleteCalls       atomic.Int32
	deleteErr         error
	deleteObservation cleanupContextObservation
	deletedObject     ctrl_client.Object
}

func (client *tunnelProxyDeleteTestClient) Delete(
	ctx context.Context,
	object ctrl_client.Object,
	_ ...ctrl_client.DeleteOption,
) error {
	client.deleteObservation.err = ctx.Err()
	client.deleteObservation.deadline, client.deleteObservation.hasDeadline = ctx.Deadline()
	client.deletedObject = object
	client.deleteCalls.Add(1)
	return client.deleteErr
}

func newServerProxyStartTestProxy(
	t *testing.T,
	name string,
) (*apiv1.ContainerNetworkTunnelProxy, string, string) {
	t.Helper()

	suffix, suffixErr := randdata.MakeRandomString(12)
	require.NoError(t, suffixErr)
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-" + string(suffix)),
		},
	}
	stdoutPath := filepath.Join(
		usvc_io.DcpTempDir(),
		fmt.Sprintf("%s_out_%s", proxy.Name, proxy.UID),
	)
	stderrPath := filepath.Join(
		usvc_io.DcpTempDir(),
		fmt.Sprintf("%s_err_%s", proxy.Name, proxy.UID),
	)
	t.Cleanup(func() {
		_ = os.Remove(stdoutPath)
		_ = os.Remove(stderrPath)
	})
	return proxy, stdoutPath, stderrPath
}

// Verifies that a server proxy configuration failure is terminal, preserves its original status error,
// and retains process identity only when rollback cannot confirm cleanup.
func TestServerProxyConfigFailurePreservesIdentityAfterCleanupFailure(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	configReadErr := errors.New("server proxy configuration is unavailable")
	stopErr := errors.New("server proxy stop failed")
	proxy, stdoutPath, stderrPath := newServerProxyStartTestProxy(t, "config-failure")
	executor := &serverProxyStartTestExecutor{
		handle:     process.NewHandle(4200, time.Unix(1000, 0).UTC()),
		stopErrors: []error{stopErr, nil},
	}
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				require.Equal(t, 1, executor.watcherCalls, "process watcher must start before configuration is read")
				require.Equal(
					t,
					process.ProcessCreationFlag(process.CreationFlagEnsureKillOnDispose),
					executor.creationFlags[0],
					"server process must be owned before configuration is read",
				)
				return dcptun.TunnelProxyConfig{}, configReadErr
			},
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
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
	require.Equal(t, fmt.Sprintf("Failed to read server proxy configuration: %v", configReadErr), proxyData.Message)
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)
	require.Equal(t, []process.ProcessCreationFlag{process.CreationFlagEnsureKillOnDispose}, executor.creationFlags)
	require.Equal(t, 1, executor.watcherCalls)

	retryStopErr := reconciler.stopServerProxyProcess(context.Background(), proxyData)
	require.NoError(t, retryStopErr)
	require.Equal(t, 1, executor.startCalls, "cleanup retry must not launch another server proxy")
	require.Equal(t, 2, executor.stopCalls)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.True(t, proxyData.ServerProxyStartupTimestamp.IsZero())
}

// Verifies that confirmed rollback after a server proxy configuration failure clears process identity
// while preserving the terminal configuration-read error.
func TestServerProxyConfigFailureClearsIdentityAfterConfirmedCleanup(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	configReadErr := errors.New("server proxy configuration is unavailable")
	proxy, stdoutPath, stderrPath := newServerProxyStartTestProxy(t, "confirmed-cleanup")
	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4201, time.Unix(1001, 0).UTC()),
	}
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				return dcptun.TunnelProxyConfig{}, configReadErr
			},
		},
		proxyData: NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)

	_, started := reconciler.startServerProxy(
		context.Background(),
		proxy,
		proxyData,
		logr.Discard(),
	)

	require.False(t, started)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, proxyData.State)
	require.Equal(t, fmt.Sprintf("Failed to read server proxy configuration: %v", configReadErr), proxyData.Message)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.True(t, proxyData.ServerProxyStartupTimestamp.IsZero())
	require.Equal(t, 1, executor.startCalls)
	require.Equal(t, 1, executor.stopCalls)
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)
}

// Verifies that failure after creating stdout removes that attempt-owned file
// without deleting a pre-existing stderr file that blocked startup.
func TestServerProxyPartialOutputCreationRemovesOnlyAttemptFile(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	proxy, stdoutPath, stderrPath := newServerProxyStartTestProxy(t, "partial-output")
	staleContents := []byte("pre-existing stderr")
	writeErr := usvc_io.WriteFile(stderrPath, staleContents, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr)

	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4206, time.Unix(1006, 0).UTC()),
	}
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
		},
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)

	_, started := reconciler.startServerProxy(context.Background(), proxy, proxyData, logr.Discard())

	require.False(t, started)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, proxyData.State)
	require.Zero(t, executor.startCalls)
	require.NoFileExists(t, stdoutPath)
	require.FileExists(t, stderrPath)
	stderrFile, openErr := usvc_io.OpenFileReadOnly(stderrPath)
	require.NoError(t, openErr)
	actualContents, readErr := io.ReadAll(stderrFile)
	require.NoError(t, readErr)
	require.NoError(t, stderrFile.Close())
	require.Equal(t, staleContents, actualContents)
}

// Verifies that StartProcess owns fail-closed process rollback while the controller
// removes only its attempt files and does not stop an unusable process identity.
func TestServerProxyStartFailureRemovesAttemptOutputFiles(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	startErr := errors.New("cleanup ownership could not be established")
	proxy, stdoutPath, stderrPath := newServerProxyStartTestProxy(t, "start-failure")
	executor := &serverProxyStartTestExecutor{
		handle:      process.NewHandle(4207, time.Unix(1007, 0).UTC()),
		startErrors: []error{startErr},
	}
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
		},
	}
	proxyData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)

	_, started := reconciler.startServerProxy(context.Background(), proxy, proxyData, logr.Discard())

	require.False(t, started)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, proxyData.State)
	require.Contains(t, proxyData.Message, startErr.Error())
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)
	require.Equal(t, []process.ProcessCreationFlag{process.CreationFlagEnsureKillOnDispose}, executor.creationFlags)
	require.Zero(t, executor.watcherCalls)
	require.Zero(t, executor.stopCalls)
}

// Verifies that configuration-read rollback removes its output artifacts so an
// independent later attempt with the same proxy identity does not hit stale files.
func TestServerProxyConfigFailureCleansFilesForIndependentSecondAttempt(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	proxy, stdoutPath, stderrPath := newServerProxyStartTestProxy(t, "config-second-attempt")
	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4208, time.Unix(1008, 0).UTC()),
	}
	var readCalls atomic.Int32
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
			readServerProxyConfig: func(context.Context, string) (dcptun.TunnelProxyConfig, error) {
				if readCalls.Add(1) == 1 {
					return dcptun.TunnelProxyConfig{}, errors.New("first configuration read failed")
				}
				return dcptun.TunnelProxyConfig{ServerControlPort: 4209}, nil
			},
		},
	}

	firstData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)
	_, firstStarted := reconciler.startServerProxy(context.Background(), proxy, firstData, logr.Discard())
	require.False(t, firstStarted)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, firstData.State)
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)

	secondData := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxy.UID)
	_, secondStarted := reconciler.startServerProxy(context.Background(), proxy, secondData, logr.Discard())
	require.True(t, secondStarted)
	require.Equal(t, int32(4209), secondData.ServerProxyControlPort)
	require.FileExists(t, stdoutPath)
	require.FileExists(t, stderrPath)
	require.Equal(t, 2, executor.startCalls)
	require.Equal(t, 2, executor.watcherCalls)

	require.NoError(t, closeAndRemoveServerProxyOutputFiles(secondData.serverStdout, secondData.serverStderr))
	secondData.serverStdout = nil
	secondData.serverStderr = nil
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)
}

// Verifies that expected rollback exits do not fail startup, stale callbacks cannot affect a replacement,
// and the current server proxy callback still records an unexpected exit.
func TestServerProxyExitCallbacksAreRunScoped(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()
	proxy, stdoutPath, stderrPath := newServerProxyStartTestProxy(t, "callback-during-rollback")
	executor := &serverProxyStartTestExecutor{
		handle:         process.NewHandle(4202, time.Unix(1002, 0).UTC()),
		exitDuringStop: true,
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
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, proxyData.State)
	require.Nil(t, proxyData.ServerProxyProcessID)
	require.Equal(t, 1, executor.stopCalls)
	require.NoFileExists(t, stdoutPath)
	require.NoFileExists(t, stderrPath)

	_ = reconciler.proxyData.Update(proxyName, proxyName, proxyData)
	reconciler.proxyData.RunDeferredOps(proxyName, proxy)
	_, currentData := reconciler.proxyData.BorrowByNamespacedName(proxyName)
	require.NotNil(t, currentData)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, currentData.State)

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
	t.Parallel()
	dcppaths.EnableTestPathProbing()

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()
	proxy, _, _ := newServerProxyStartTestProxy(t, "exit-before-startup-publication")
	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4204, time.Unix(1004, 0).UTC()),
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

// Verifies that publication failure releases the startup lease and that an empty result
// does not require configured process or Kubernetes cleanup dependencies.
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
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	client := &tunnelProxyDeleteTestClient{Client: baseClient}
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
		proxyData:             NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData, *apiv1.ContainerNetworkTunnelProxy](),
		workQueue:             resiliency.NewWorkQueue(testCtx, 1),
		discardedCleanupTasks: concurrency.NewCountdownLatch(),
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
	deleteErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		return client.deleteCalls.Load() == 1, nil
	})
	require.NoError(t, deleteErr)
	require.Equal(t, int32(1), client.deleteCalls.Load())
	var releaseAfterCleanup func()
	acquireErr := wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
		var leaseAcquired bool
		releaseAfterCleanup, leaseAcquired = current.startup.TryAcquire(testCtx)
		return leaseAcquired, nil
	})
	require.NoError(t, acquireErr)
	releaseAfterCleanup()
}

// Verifies that cleanup accepted behind a busy shared worker is still owned when controller
// cancellation drops the queued item, producing exactly one stop and one delete submission.
func TestDiscardedProxyCleanupSurvivesCancellationAfterAcceptance(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	lifetimeCtx, cancelLifetime := context.WithCancel(testCtx)
	defer cancelLifetime()
	workQueue := resiliency.NewWorkQueue(lifetimeCtx, 1)
	blockingWorkStarted := make(chan struct{})
	blockingWorkErr := workQueue.Enqueue(func(ctx context.Context) {
		close(blockingWorkStarted)
		<-ctx.Done()
	})
	require.NoError(t, blockingWorkErr)
	select {
	case <-blockingWorkStarted:
	case <-testCtx.Done():
		t.Fatal("blocking work did not start")
	}

	executor := &serverProxyStartTestExecutor{
		handle: process.NewHandle(4302, time.Unix(1302, 0).UTC()),
	}
	client := &tunnelProxyDeleteTestClient{}
	reconciler := &ContainerNetworkTunnelProxyReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](
			client,
			client,
			logr.Discard(),
			lifetimeCtx,
		),
		config: ContainerNetworkTunnelProxyReconcilerConfig{
			ProcessExecutor: executor,
		},
		workQueue:             workQueue,
		discardedCleanupTasks: concurrency.NewCountdownLatch(),
	}
	proxyUID := types.UID("discarded-cancellation")
	result := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateRunning, proxyUID)
	serverPID := int64(executor.handle.Pid)
	result.ServerProxyProcessID = &serverPID
	result.ServerProxyStartupTimestamp = metav1.NewMicroTime(executor.handle.IdentityTime)

	reconciler.queueDiscardedProxyPairCleanup(proxyUID, result, nil, logr.Discard())
	cancelLifetime()
	reconciler.discardedCleanupTasks.Close()
	reconciler.discardedCleanupTasks.Wait()

	require.Equal(t, 1, executor.stopCalls)
	require.Equal(t, int32(1), client.deleteCalls.Load())
}

// Verifies that discarded startup cleanup makes one detached, bounded process-stop attempt
// and one independent delete submission, then closes local output handles without retrying.
func TestDiscardedProxyCleanupMakesOneIndependentAttemptPerStage(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	parentCtx, cancelParent := context.WithCancel(testCtx)
	cancelParent()

	stopErr := errors.New("server stop failed")
	deleteErr := errors.New("delete submission failed")
	executor := &serverProxyStartTestExecutor{
		handle:     process.NewHandle(4301, time.Unix(1301, 0).UTC()),
		stopErrors: []error{stopErr},
	}
	client := &tunnelProxyDeleteTestClient{deleteErr: deleteErr}
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
	}
	proxyUID := types.UID("discarded-one-shot")
	pd := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateRunning, proxyUID)
	serverPID := int64(executor.handle.Pid)
	pd.ServerProxyProcessID = &serverPID
	pd.ServerProxyStartupTimestamp = metav1.NewMicroTime(executor.handle.IdentityTime)

	stdoutPath := filepath.Join(t.TempDir(), "server-stdout")
	stdoutFile, stdoutErr := usvc_io.CreateNewFile(stdoutPath, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdoutErr)
	t.Cleanup(func() {
		_ = stdoutFile.Close()
		_ = os.Remove(stdoutPath)
	})
	stderrPath := filepath.Join(t.TempDir(), "server-stderr")
	stderrFile, stderrErr := usvc_io.CreateNewFile(stderrPath, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stderrErr)
	t.Cleanup(func() {
		_ = stderrFile.Close()
		_ = os.Remove(stderrPath)
	})
	pd.serverStdout = stdoutFile
	pd.serverStderr = stderrFile

	cleanupConfirmed := reconciler.cleanupDiscardedProxyPair(
		parentCtx,
		pd,
		proxyUID,
		logr.Discard(),
	)

	require.False(t, cleanupConfirmed)
	require.Equal(t, 1, executor.stopCalls)
	require.NoError(t, executor.stopObservation.err)
	require.True(t, executor.stopObservation.hasDeadline)
	require.Equal(t, int32(1), client.deleteCalls.Load())
	require.NoError(t, client.deleteObservation.err)
	require.True(t, client.deleteObservation.hasDeadline)
	require.IsType(t, &apiv2.PhysicalContainer{}, client.deletedObject)
	require.Equal(
		t,
		tunnelProxyPhysicalContainerNameForUID(proxyUID),
		ctrl_client.ObjectKeyFromObject(client.deletedObject),
	)
	_, stdoutWriteErr := stdoutFile.Write([]byte("closed"))
	require.ErrorIs(t, stdoutWriteErr, os.ErrClosed)
	_, stderrWriteErr := stderrFile.Write([]byte("closed"))
	require.ErrorIs(t, stderrWriteErr, os.ErrClosed)
	require.NotNil(t, pd.ServerProxyProcessID)
}

// Verifies that an accepted or already-completed PhysicalContainer deletion is treated
// as a successful handoff without polling for finalizer completion.
func TestDiscardedProxyCleanupTreatsDeleteSubmissionAsHandoff(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		deleteErr error
	}{
		{name: "delete accepted"},
		{
			name: "container already absent",
			deleteErr: apierrors.NewNotFound(
				schema.GroupResource{Group: apiv2.GroupVersion.Group, Resource: "physicalcontainers"},
				"missing",
			),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
			defer testCancel()
			client := &tunnelProxyDeleteTestClient{deleteErr: testCase.deleteErr}
			reconciler := &ContainerNetworkTunnelProxyReconciler{
				ReconcilerBase: NewReconcilerBase[apiv1.ContainerNetworkTunnelProxy](
					client,
					client,
					logr.Discard(),
					testCtx,
				),
				config: ContainerNetworkTunnelProxyReconcilerConfig{
					ProcessExecutor: &serverProxyStartTestExecutor{},
				},
			}
			proxyUID := types.UID("delete-handoff-" + testCase.name)
			pd := newContainerNetworkTunnelProxyData(apiv1.ContainerNetworkTunnelProxyStateStarting, proxyUID)

			cleanupConfirmed := reconciler.cleanupDiscardedProxyPair(
				testCtx,
				pd,
				proxyUID,
				logr.Discard(),
			)

			require.True(t, cleanupConfirmed)
			require.Equal(t, int32(1), client.deleteCalls.Load())
		})
	}
}
