/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

const execCleanupFinalizer = apiv2.GroupName + "/physicalcontainerexec-reconciler"

type execBackingClient struct {
	ctrl_client.Client
	blockFinalStatus    atomic.Bool
	finalStatus         chan *apiv2.PhysicalContainerExec
	finalStatusOnce     sync.Once
	loseHoldResponse    atomic.Bool
	holdResponseLost    atomic.Bool
	loseCleanupResponse atomic.Bool
	cleanupResponseLost atomic.Bool
	failObservation     atomic.Bool
	failNamespace       atomic.Bool
	observation         chan struct{}
	observationOnce     sync.Once
}

func (client *execBackingClient) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, namespace := obj.(*apiv2.Namespace); namespace && client.failNamespace.Load() {
		return errors.New("execution namespace lookup unavailable")
	}
	return client.Client.Get(ctx, key, obj, options...)
}

func (client *execBackingClient) Status() ctrl_client.SubResourceWriter {
	return &execBackingStatusWriter{SubResourceWriter: client.Client.Status(), client: client}
}

func (client *execBackingClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, executions := list.(*apiv2.PhysicalContainerExecList); executions && client.failObservation.Load() {
		client.observationOnce.Do(func() { close(client.observation) })
		return errors.New("execution observation unavailable")
	}
	return client.Client.List(ctx, list, options...)
}

func (client *execBackingClient) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption) error {
	if physical, execution := obj.(*apiv2.PhysicalContainerExec); execution && !physical.DeletionTimestamp.IsZero() {
		if !slices.Contains(physical.Finalizers, execCleanupFinalizer) {
			var stored apiv2.PhysicalContainerExec
			getErr := client.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(physical), &stored)
			if getErr != nil {
				return getErr
			}
			if !stored.Status.LogsComplete || stored.Status.FinishedAt.IsZero() {
				return errors.New("execution cleanup finalizer released before final status was durable")
			}
			if slices.Contains(physical.Finalizers, controllers.LogicalBackingFinalizer) &&
				client.loseCleanupResponse.CompareAndSwap(true, false) {
				patchErr := client.Client.Patch(ctx, obj, patch, options...)
				if patchErr != nil {
					return patchErr
				}
				client.cleanupResponseLost.Store(true)
				return errors.New("applied execution cleanup response lost")
			}
		}
		if !slices.Contains(physical.Finalizers, controllers.LogicalBackingFinalizer) &&
			client.loseHoldResponse.CompareAndSwap(true, false) {
			patchErr := client.Client.Patch(ctx, obj, patch, options...)
			if patchErr != nil {
				return patchErr
			}
			client.holdResponseLost.Store(true)
			return errors.New("applied execution hold release response lost")
		}
	}
	return client.Client.Patch(ctx, obj, patch, options...)
}

type execBackingStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *execBackingClient
}

func (writer *execBackingStatusWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	if physical, execution := obj.(*apiv2.PhysicalContainerExec); execution &&
		!physical.DeletionTimestamp.IsZero() && physical.Status.LogsComplete && writer.client.blockFinalStatus.Load() {
		writer.client.finalStatusOnce.Do(func() { writer.client.finalStatus <- physical.DeepCopy() })
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "physicalcontainerexecs"},
			physical.Name, errors.New("execution final status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

// Verifies that physical exec cleanup preserves final results through status conflicts,
// retains the logical hold until cleanup is durable, and recovers lost cleanup/hold-release responses.
func TestV2ContainerExecDeletionPreservesDurableDisposition(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *execBackingClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &execBackingClient{Client: client, finalStatus: make(chan *apiv2.PhysicalContainerExec, 1)}
			return decorated
		}})
	require.NoError(t, startupErr)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-durable-cleanup")
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "exec-durable-cleanup"}}}
	require.NoError(t, server.Client.Create(ctx, logical))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalExecRef, Namespace: target.Namespace}}
	require.NoError(t, server.Client.Get(ctx, physical.NamespacedName(), physical))
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.Contains(t, physical.Finalizers, execCleanupFinalizer)
	decorated.blockFinalStatus.Store(true)
	defer decorated.blockFinalStatus.Store(false)
	decorated.loseHoldResponse.Store(true)
	decorated.loseCleanupResponse.Store(true)
	require.NoError(t, server.Client.Delete(ctx, logical))
	var final *apiv2.PhysicalContainerExec
	select {
	case final = <-decorated.finalStatus:
	case <-ctx.Done():
		t.Fatal("physical cleanup did not attempt final status")
	}
	require.NoError(t, server.Client.Get(ctx, physical.NamespacedName(), physical))
	require.False(t, physical.DeletionTimestamp.IsZero(), "deletion must be requested before cleanup observation")
	require.Contains(t, physical.Finalizers, execCleanupFinalizer)
	require.Contains(t, physical.Finalizers, controllers.LogicalBackingFinalizer)
	require.False(t, physical.Status.LogsComplete)
	_, stdoutErr := os.Stat(final.Status.StdoutFile)
	require.NoError(t, stdoutErr, "capture must survive failed final status projection")
	decorated.blockFinalStatus.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	require.True(t, decorated.holdResponseLost.Load())
	require.True(t, decorated.cleanupResponseLost.Load())
	orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, simulated)
	require.Equal(t, 1, orchestrator.ExecContainerCallCount(logical.Spec.Execution.Command))
}

// Verifies that physical exec retains its cleanup finalizer and reports disposal failures,
// then retries capture disposal after the filesystem obstruction is removed.
func TestV2ContainerExecRetriesCaptureDisposal(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *execBackingClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &execBackingClient{Client: client, finalStatus: make(chan *apiv2.PhysicalContainerExec, 1)}
			return decorated
		}})
	require.NoError(t, startupErr)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-capture-disposal")
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef,
			Execution: apiv2.ContainerExecConfig{Command: "exec-capture-disposal"}}}
	require.NoError(t, server.Client.Create(ctx, physical))
	waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	decorated.blockFinalStatus.Store(true)
	defer decorated.blockFinalStatus.Store(false)
	require.NoError(t, server.Client.Delete(ctx, physical))
	var final *apiv2.PhysicalContainerExec
	select {
	case final = <-decorated.finalStatus:
	case <-ctx.Done():
		t.Fatal("physical cleanup did not attempt final status")
	}
	path := final.Status.StdoutFile
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	obstruction := filepath.Join(path, "obstruction")
	require.NoError(t, os.Mkdir(obstruction, 0700))
	defer func() {
		for _, directory := range []string{obstruction, path} {
			removeErr := os.Remove(directory)
			require.True(t, removeErr == nil || errors.Is(removeErr, os.ErrNotExist), "%v", removeErr)
		}
	}()
	decorated.blockFinalStatus.Store(false)
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		gvr := physical.GetGroupVersionResource()
		output, readErr := server.RestClient.Get().
			AbsPath("/apis", gvr.Group, gvr.Version, "namespaces", physical.Namespace, gvr.Resource, physical.Name, apiv2.LogSubresourceName).
			Param("source", "system").DoRaw(ctx)
		if readErr != nil {
			return false, readErr
		}
		return strings.Contains(string(output), "Failed to dispose execution output capture"), nil
	}))
	require.NoError(t, server.Client.Get(ctx, physical.NamespacedName(), physical))
	require.True(t, physical.Status.LogsComplete)
	require.Contains(t, physical.Finalizers, execCleanupFinalizer)
	require.NoError(t, os.Remove(obstruction))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	_, captureErr := os.Stat(path)
	require.ErrorIs(t, captureErr, os.ErrNotExist)
}

// Verifies that logical exec retries observation errors without losing its selected backing
// or launching another execution when cached owner observation resumes.
func TestV2LogicalContainerExecRetriesOwnerObservation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *execBackingClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &execBackingClient{Client: client, observation: make(chan struct{})}
			return decorated
		}})
	require.NoError(t, startupErr)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-observation-retry")
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name, Execution: apiv2.ContainerExecConfig{Command: "exec-observation-retry"}}}
	require.NoError(t, server.Client.Create(ctx, logical))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	decorated.failObservation.Store(true)
	defer decorated.failObservation.Store(false)
	require.NoError(t, server.Client.Patch(ctx, logical,
		ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"test.dcp.microsoft.com/reconcile":"observe"}}}`))))
	waitContainerExecSignal(t, ctx, decorated.observation)
	waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhasePending, nil
	})
	decorated.failObservation.Store(false)
	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
	})
	require.Equal(t, running.Status.PhysicalExecRef, recovered.Status.PhysicalExecRef)
	require.Equal(t, running.Status.RuntimeLogs.UID, recovered.Status.RuntimeLogs.UID)
	orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, simulated)
	require.Equal(t, 1, orchestrator.ExecContainerCallCount(logical.Spec.Execution.Command))
}

// Verifies that physical exec deletion bypasses failed namespace admission and durably
// publishes cancelled completion without launching a command or allocating normal execution state.
func TestV2ContainerExecCancelsBeforeNamespaceAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *execBackingClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, containerExecTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &execBackingClient{Client: client}
			return decorated
		}})
	require.NoError(t, startupErr)
	target := createV2ExecTarget(t, ctx, server.Client, "v2-exec-namespace-admission")
	decorated.failNamespace.Store(true)
	defer decorated.failNamespace.Store(false)
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", Namespace: target.Namespace},
		Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: target.Status.PhysicalContainerRef,
			Execution: apiv2.ContainerExecConfig{Command: "exec-before-admission"}}}
	require.NoError(t, server.Client.Create(ctx, physical))
	waitObjectAssumesStateEx(t, ctx, server.Client, physical.NamespacedName(), func(current *apiv2.PhysicalContainerExec) (bool, error) {
		return current.Status.Phase == apiv2.ContainerExecPhasePending && len(current.Status.Conditions) != 0 &&
			current.Status.Conditions[0].Reason == string(apiv2.PhysicalResourceReasonNamespaceLookupFailed), nil
	})
	require.NoError(t, server.Client.Delete(ctx, physical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, simulated)
	require.Zero(t, orchestrator.ExecContainerCallCount(physical.Spec.Execution.Command))
}
