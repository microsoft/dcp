/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientrest "k8s.io/client-go/rest"
	clientwebsocket "k8s.io/client-go/transport/websocket"
	"k8s.io/client-go/util/retry"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/projectdebug"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type projectTargetExecutor struct {
	process.Executor
	lock         sync.Mutex
	stops        map[process.ProcessHandle]int
	refuse       atomic.Bool
	replaced     atomic.Bool
	unobservable atomic.Bool
	lookups      atomic.Int32
	stopEntered  chan process.ProcessHandle
	stopRelease  <-chan struct{}
	adapter      atomic.Pointer[process.ProcessHandle]
	orderFailed  atomic.Bool
	activeStops  atomic.Int32
	peakStops    atomic.Int32
}

func (executor *projectTargetExecutor) stopCounts() map[process.ProcessHandle]int {
	executor.lock.Lock()
	defer executor.lock.Unlock()
	return maps.Clone(executor.stops)
}

func (executor *projectTargetExecutor) FindProcessHandle(process.Pid_t) (process.ProcessHandle, error) {
	executor.lookups.Add(1)
	return process.ProcessHandle{}, errors.New("must reuse the registered process identity")
}

func (executor *projectTargetExecutor) CheckProcessRunning(handle process.ProcessHandle) error {
	if executor.replaced.Load() {
		return process.ErrProcessIdentityMismatch
	}
	if executor.unobservable.Load() {
		return process.ErrProcessIdentityUnavailable
	}
	return executor.Executor.CheckProcessRunning(handle)
}

func (executor *projectTargetExecutor) StopProcess(ctx context.Context, handle process.ProcessHandle, options ...process.ProcessStopOption) error {
	executor.lock.Lock()
	executor.stops[handle]++
	executor.lock.Unlock()
	if adapter := executor.adapter.Load(); adapter != nil && !process.IsProcessGoneErr(executor.Executor.CheckProcessRunning(*adapter)) {
		executor.orderFailed.Store(true)
	}
	if executor.stopEntered != nil {
		active := executor.activeStops.Add(1)
		defer executor.activeStops.Add(-1)
		for peak := executor.peakStops.Load(); active > peak; peak = executor.peakStops.Load() {
			if executor.peakStops.CompareAndSwap(peak, active) {
				break
			}
		}
		executor.stopEntered <- handle
		select {
		case <-executor.stopRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if executor.refuse.Load() {
		return errors.New("injected target stop refusal")
	}
	if executor.replaced.Load() {
		return process.ErrProcessIdentityMismatch
	}
	executor.unobservable.Store(false)
	return executor.Executor.StopProcess(ctx, handle, options...)
}

type projectTargetConflictClient struct {
	ctrl_client.Client
	conflicted          atomic.Bool
	finalizerConflicted atomic.Bool
}

func (client *projectTargetConflictClient) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption) error {
	if project, valid := obj.(*apiv2.Project); valid && !project.DeletionTimestamp.IsZero() &&
		!slices.Contains(project.Finalizers, "usvc-dev.developer.microsoft.com/project-reconciler") &&
		client.finalizerConflicted.CompareAndSwap(false, true) {
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "projects"},
			project.Name, errors.New("injected finalizer release conflict"))
	}
	return client.Client.Patch(ctx, obj, patch, options...)
}

func (client *projectTargetConflictClient) Status() ctrl_client.SubResourceWriter {
	return &projectTargetConflictWriter{SubResourceWriter: client.Client.Status(), client: client}
}

type projectTargetConflictWriter struct {
	ctrl_client.SubResourceWriter
	client *projectTargetConflictClient
}

func (writer *projectTargetConflictWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	if project, valid := obj.(*apiv2.Project); valid && project.Status.Debug != nil {
		for _, session := range project.Status.Debug.Sessions {
			for _, target := range session.Processes {
				if target.Observation.Cleanup != "" && writer.client.conflicted.CompareAndSwap(false, true) {
					return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "projects"},
						project.Name, errors.New("injected target cleanup status conflict"))
				}
			}
		}
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

func startProjectTarget(t *testing.T, ctx context.Context, executor process.Executor, tool string, arguments ...string) process.ProcessHandle {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(tool, append([]string{"--ready-file", ready}, arguments...)...)
	handle, enableWait, startErr := executor.StartProcess(ctx, cmd, nil, process.CreationFlagEnsureKillOnDispose, nil)
	require.NoError(t, startErr)
	enableWait()
	t.Cleanup(func() {
		cleanupCtx, cancel := process.WithDetachedStopTimeout(ctx)
		defer cancel()
		stopErr := executor.StopProcess(cleanupCtx, handle)
		require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		_, statErr := os.Stat(ready)
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return statErr == nil, statErr
	}))
	return handle
}

func reportProjectTarget(t *testing.T, connection *websocket.Conn, pid process.Pid_t) {
	t.Helper()
	body := fmt.Sprintf(`{"seq":12,"type":"request","command":"reportProcess","arguments":{"name":"target","systemProcessId":%d}}`, pid)
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
	event := readProjectDebugMessage(t, connection)
	require.JSONEq(t, `"process"`, string(event["event"]))
	response := readProjectDebugMessage(t, connection)
	require.JSONEq(t, `"response"`, string(response["type"]))
}

func connectProjectDebug(t *testing.T, ctx context.Context, config *clientrest.Config, project *apiv2.Project) (*websocket.Conn, error) {
	t.Helper()
	transport, holder, transportErr := clientwebsocket.RoundTripperFor(config)
	if transportErr != nil {
		return nil, transportErr
	}
	request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
		config.Host+"/apis/"+apiv2.GroupVersion.String()+"/namespaces/"+project.Namespace+"/projects/"+project.Name+"/debug", nil)
	if requestErr != nil {
		return nil, requestErr
	}
	if token := os.Getenv(projectdebug.DCP_DEBUG_SESSION_TOKEN); token != "" {
		request.Header.Set("X-DCP-Debug-Session-Token", token)
	}
	connection, connectionErr := clientwebsocket.Negotiate(transport, holder, request, "dap")
	if connectionErr == nil {
		if deadline, exists := ctx.Deadline(); exists {
			if deadlineErr := connection.SetReadDeadline(deadline); deadlineErr != nil {
				_ = connection.Close()
				return nil, deadlineErr
			}
		}
	}
	return connection, connectionErr
}

func releaseProjectInspection(t *testing.T, ctx context.Context, client ctrl_client.Client, project *apiv2.Project) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if getErr := client.Get(ctx, project.NamespacedName(), project); getErr != nil {
			return getErr
		}
		project.Finalizers = slices.DeleteFunc(project.Finalizers, func(finalizer string) bool { return finalizer == "test/inspect-target-cleanup" })
		return client.Update(ctx, project)
	}))
}

// Verifies that sessions retain multiple identity-safe target registrations after disconnect,
// deduplicate cleanup across sessions and publish cleanup through status conflicts on stop or namespace deletion.
func TestV2ProjectRetainsAndCleansSessionProcesses(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"stop", "namespace", "unconfirmed", "identity-changed", "unobservable"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var tracked *projectTargetExecutor
			var conflicts *projectTargetConflictClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, ProjectController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateProjectProcessExecutor: func(executor process.Executor) process.Executor {
						tracked = &projectTargetExecutor{Executor: executor, stops: make(map[process.ProcessHandle]int)}
						return tracked
					},
					DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						conflicts = &projectTargetConflictClient{Client: client}
						return conflicts
					},
				})
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-targets-"+scenario)
			tool, toolErr := internaltest.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			ownerExecutor := process.NewOSExecutor(logr.Discard())
			t.Cleanup(ownerExecutor.Dispose)
			handles := []process.ProcessHandle{
				startProjectTarget(t, ctx, ownerExecutor, tool),
				startProjectTarget(t, ctx, ownerExecutor, tool),
			}
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
					Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
						ExecutablePath: "adapter-started", AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
					}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}},
				}}
			if scenario == "namespace" || scenario == "unconfirmed" {
				project.Finalizers = []string{"test/inspect-target-cleanup"}
			}
			require.NoError(t, server.Client.Create(ctx, project))
			waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			reportProjectTarget(t, connection, handles[0].Pid)
			reportProjectTarget(t, connection, handles[0].Pid)
			reportProjectTarget(t, connection, handles[1].Pid)
			second, secondErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, secondErr)
			defer second.Close()
			reportProjectTarget(t, second, handles[0].Pid)
			registered := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 2 {
					return false, nil
				}
				for _, session := range current.Status.Debug.Sessions {
					for _, target := range session.Processes {
						if target.Observation.Phase != apiv2.ProjectDebugProcessRunning {
							return false, nil
						}
					}
				}
				return len(current.Status.Debug.Sessions[0].Processes) == 2 && len(current.Status.Debug.Sessions[1].Processes) == 1, nil
			})
			require.Equal(t, int64(handles[0].Pid), *registered.Status.Debug.Sessions[0].Processes[0].PID)
			require.NoError(t, connection.Close())
			require.NoError(t, second.Close())
			idle := waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			require.Len(t, idle.Status.Debug.Sessions[0].Processes, 2)
			for _, handle := range handles {
				require.NoError(t, ownerExecutor.CheckProcessRunning(handle), "session closure must retain independent targets")
			}
			if scenario == "unobservable" {
				tracked.unobservable.Store(true)
				waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseReady &&
						current.Status.Debug.Sessions[0].Processes[0].Observation.Phase == apiv2.ProjectDebugProcessUnknown, nil
				})
			}
			tracked.refuse.Store(scenario == "unconfirmed")
			tracked.replaced.Store(scenario == "identity-changed")
			if scenario == "namespace" {
				require.NoError(t, server.Client.Delete(ctx, namespace))
			} else {
				require.NoError(t, server.Client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			}
			cleaned := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				if current.Status.Debug == nil {
					return false, nil
				}
				for _, session := range current.Status.Debug.Sessions {
					for _, target := range session.Processes {
						if target.Observation.Cleanup == "" {
							return false, nil
						}
					}
				}
				return true, nil
			})
			require.True(t, conflicts.conflicted.Load())
			for _, session := range cleaned.Status.Debug.Sessions {
				for _, target := range session.Processes {
					if scenario == "unconfirmed" {
						require.Equal(t, apiv2.ProjectDebugProcessCleanupUnconfirmed, target.Observation.Cleanup)
					} else {
						require.Equal(t, apiv2.ProjectDebugProcessCleanupComplete, target.Observation.Cleanup)
						require.Equal(t, apiv2.ProjectDebugProcessExited, target.Observation.Phase)
					}
				}
			}
			counts := tracked.stopCounts()
			for _, count := range counts {
				require.Equal(t, 1, count, "a shared target must be stopped only once")
			}
			if scenario == "identity-changed" {
				require.Empty(t, counts, "a process whose identity changed must not receive a stop request")
			} else {
				require.Len(t, counts, 2)
			}
			for handle := range counts {
				require.True(t, (handle.Pid == handles[0].Pid && handle.IdentityTime.Equal(handles[0].IdentityTime)) ||
					(handle.Pid == handles[1].Pid && handle.IdentityTime.Equal(handles[1].IdentityTime)),
					"cleanup must use the originally captured identity")
			}
			require.Zero(t, tracked.lookups.Load())
			if scenario == "namespace" {
				releaseProjectInspection(t, ctx, server.Client, project)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
				require.True(t, conflicts.finalizerConflicted.Load())
			} else {
				waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseStopped)
			}
			for _, handle := range handles {
				if scenario == "unconfirmed" || scenario == "identity-changed" {
					require.NoError(t, ownerExecutor.CheckProcessRunning(handle))
				} else {
					require.True(t, process.IsProcessGoneErr(ownerExecutor.CheckProcessRunning(handle)))
				}
			}
			if scenario == "unconfirmed" {
				tracked.refuse.Store(false)
				require.NoError(t, server.Client.Delete(ctx, project))
				waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Debug.Sessions[0].Processes[0].Observation.Cleanup == apiv2.ProjectDebugProcessCleanupComplete &&
						!slices.Contains(current.Finalizers, "usvc-dev.developer.microsoft.com/project-reconciler"), nil
				})
				for _, count := range tracked.stopCounts() {
					require.Equal(t, 2, count, "deletion must retry a previously unconfirmed stop")
				}
				releaseProjectInspection(t, ctx, server.Client, project)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
			}
		})
	}
}

// Verifies that Project cleans independent target trees concurrently, serializes registered
// ancestors and descendants, respects the shared concurrency limit, and skips children removed by a parent.
func TestV2ProjectCleansIndependentTargetTrees(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	release := make(chan struct{})
	releaseStops := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseStops)
	var tracked *projectTargetExecutor
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, ProjectController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateProjectProcessExecutor: func(executor process.Executor) process.Executor {
			tracked = &projectTargetExecutor{Executor: executor, stops: make(map[process.ProcessHandle]int),
				stopEntered: make(chan process.ProcessHandle, int(controllers.MaxConcurrentReconciles)+2), stopRelease: release}
			return tracked
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-parallel-targets")
	tool, toolErr := internaltest.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	ownerExecutor := process.NewOSExecutor(logr.Discard())
	t.Cleanup(ownerExecutor.Dispose)
	t.Cleanup(releaseStops)
	parent := startProjectTarget(t, ctx, ownerExecutor, tool, "--child-spec", "1")
	tree, treeErr := process.GetProcessTree(ctx, parent)
	require.NoError(t, treeErr)
	require.Len(t, tree, 2)
	child := tree[1]
	handles := []process.ProcessHandle{child, parent}
	for range controllers.MaxConcurrentReconciles {
		handles = append(handles, startProjectTarget(t, ctx, ownerExecutor, tool))
	}
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
			Process:      &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "adapter-started"}},
			DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}}}}
	require.NoError(t, server.Client.Create(ctx, project))
	waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
	connection, connectionErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
	require.NoError(t, connectionErr)
	defer connection.Close()
	for _, handle := range handles {
		reportProjectTarget(t, connection, handle.Pid)
	}
	waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1 &&
			len(current.Status.Debug.Sessions[0].Processes) == len(handles), nil
	})
	require.NoError(t, server.Client.Patch(ctx, project, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	started := make(map[process.Pid_t]bool)
	for range controllers.MaxConcurrentReconciles {
		select {
		case handle := <-tracked.stopEntered:
			started[handle.Pid] = true
		case <-ctx.Done():
			t.Fatal("independent target trees did not begin cleanup concurrently")
		}
	}
	require.True(t, started[parent.Pid])
	require.Len(t, started, int(controllers.MaxConcurrentReconciles))
	require.False(t, started[child.Pid])
	releaseStops()
	stopped := waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseStopped)
	for _, target := range stopped.Status.Debug.Sessions[0].Processes {
		require.Equal(t, apiv2.ProjectDebugProcessCleanupComplete, target.Observation.Cleanup)
	}
	require.Len(t, tracked.stopCounts(), len(handles)-1)
	require.EqualValues(t, controllers.MaxConcurrentReconciles, tracked.peakStops.Load())
	require.NoError(t, server.Client.Delete(ctx, project))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
}
