/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that a shared host provider preserves an incomplete-descendant cleanup warning
// after confirmed root exit while allowing a subsequent provider to launch.
func TestProjectDebugReleasePreservesDescendantCleanupWarning(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	apiClient, writer := startProjectDebugStatusServer(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "listen")
	executor := &adapterServingExecutor{
		TestProcessExecutor: internaltest.NewTestProcessExecutor(context.WithoutCancel(ctx)),
		stops:               make(map[process.Pid_t]func()),
	}
	t.Cleanup(executor.Dispose)
	debugRuntime := newProjectDebugRuntime(ctx, executor, writer, logr.Discard())
	execution, transport, release, acquireErr := debugRuntime.acquire(ctx, project)
	require.NoError(t, acquireErr)
	require.NoError(t, transport.Close())
	executor.incompleteTree.Store(true)
	require.ErrorIs(t, release(), process.ErrIncompleteProcessTree)
	require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(execution.handle)))
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Executions, 1)
	completed := project.Status.Debug.Executions[0]
	require.Equal(t, apiv2.ProjectDebugPhaseExited, completed.Phase)
	require.False(t, completed.FinishedAt.IsZero())
	require.Len(t, completed.Conditions, 1)
	require.Equal(t, "DescendantCleanupUnconfirmed", completed.Conditions[0].Reason)
	require.Contains(t, completed.Conditions[0].Message, process.ErrIncompleteProcessTree.Error())

	executor.incompleteTree.Store(false)
	replacement, replacementTransport, releaseReplacement, replacementErr := debugRuntime.acquire(ctx, project)
	require.NoError(t, replacementErr)
	require.NotEqual(t, execution.status.ID, replacement.status.ID)
	require.NoError(t, replacementTransport.Close())
	require.NoError(t, releaseReplacement())
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	require.Len(t, project.Status.Debug.Executions, 2)
	require.Equal(t, "DescendantCleanupUnconfirmed", project.Status.Debug.Executions[0].Conditions[0].Reason)
}

// Verifies that acquisition waits behind stopping host/container providers without launching,
// cancellation preserves the fence, and a waiting acquisition proceeds when a ready provider replaces it.
func TestProjectDebugAcquireFencesStoppingProvider(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"host", "container"} {
		t.Run(mode, func(subtest *testing.T) {
			testCtx, cancelTest := testutil.GetTestContext(subtest, 0)
			deadline, hasDeadline := testCtx.Deadline()
			budget := time.Until(deadline)
			cancelTest()
			synctest.Test(subtest, func(test *testing.T) {
				ctx := test.Context()
				if hasDeadline {
					boundedCtx, cancelBounded := context.WithTimeout(ctx, budget)
					defer cancelBounded()
					ctx = boundedCtx
				}
				executor := internaltest.NewTestProcessExecutor(ctx)
				defer executor.Dispose()
				debugRuntime := newProjectDebugRuntime(ctx, executor, &projectDebugStatusWriter{}, logr.Discard())
				project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "debug", UID: "project-uid"},
					Spec: apiv2.ProjectSpec{DebugAdapter: &apiv2.DebugAdapterConfig{Mode: apiv2.DebugAdapterModeTCPConnect}}}
				if mode == "container" {
					project.Spec.Container = &apiv2.ProjectContainerTemplate{}
					project.Status.ContainerID = "prepared-container"
					project.Status.DebugAdapterAddress = "prepared-endpoint"
				} else {
					project.Spec.Process = &apiv2.ProjectProcessTemplate{}
				}
				fencedExecution := &projectDebugExecution{
					containerID: project.Status.ContainerID, address: project.Status.DebugAdapterAddress,
				}
				fenced := &projectDebugProvider{
					stopping: true, stopped: make(chan struct{}), ready: make(chan struct{}),
					execution: fencedExecution, initial: &projectDebugTransport{},
				}
				close(fenced.ready)
				debugRuntime.providers[project.UID] = fenced
				type acquisition struct {
					execution *projectDebugExecution
					transport *projectDebugTransport
					release   func() error
					err       error
				}
				results := make(chan acquisition, 1)
				acquire := func(acquireCtx context.Context) {
					execution, transport, release, acquireErr := debugRuntime.acquire(acquireCtx, project)
					results <- acquisition{execution, transport, release, acquireErr}
				}
				assertFenced := func() {
					test.Helper()
					synctest.Wait()
					select {
					case result := <-results:
						test.Fatalf("acquisition completed while provider remained fenced: %v", result.err)
					default:
					}
					require.Same(test, fenced, debugRuntime.providers[project.UID])
					require.Zero(test, fenced.refs)
					require.Empty(test, executor.Executions)
					require.Empty(test, debugRuntime.launches)
				}
				requestCtx, cancelRequest := context.WithCancel(ctx)
				defer cancelRequest()
				go acquire(requestCtx)
				assertFenced()
				cancelRequest()
				select {
				case canceled := <-results:
					require.ErrorIs(test, canceled.err, context.Canceled)
					require.Nil(test, canceled.execution)
					require.Nil(test, canceled.transport)
					require.Nil(test, canceled.release)
				case <-ctx.Done():
					test.Fatal("fenced acquisition did not return after cancellation")
				}
				require.Same(test, fenced, debugRuntime.providers[project.UID])
				select {
				case <-fenced.stopped:
					test.Fatal("request cancellation released the provider fence")
				default:
				}

				go acquire(ctx)
				assertFenced()
				execution := &projectDebugExecution{
					containerID: project.Status.ContainerID, address: project.Status.DebugAdapterAddress,
				}
				transport := &projectDebugTransport{}
				replacement := &projectDebugProvider{
					ready: make(chan struct{}), stopped: make(chan struct{}),
					execution: execution, initial: transport, refs: 1,
				}
				close(replacement.ready)
				debugRuntime.mu.Lock()
				debugRuntime.providers[project.UID] = replacement
				close(fenced.stopped)
				debugRuntime.mu.Unlock()
				select {
				case acquired := <-results:
					require.NoError(test, acquired.err)
					require.Same(test, execution, acquired.execution)
					require.Same(test, transport, acquired.transport)
					require.NotNil(test, acquired.release)
					require.Equal(test, 2, replacement.refs)
					require.NoError(test, acquired.release())
					require.Equal(test, 1, replacement.refs)
				case <-ctx.Done():
					test.Fatal("ready replacement did not release the waiting acquisition")
				}
				require.Empty(test, executor.Executions)
				require.Empty(test, debugRuntime.launches)
			})
		})
	}
}

// Verifies that redialingAdapterConnection replays client output on a new connection when a runtime port proxy
// closes the first connection before the adapter responds, and fails once its deadline expires without a response.
func TestRedialingAdapterConnection(t *testing.T) {
	for _, scenario := range []string{"proxy-close", "never-listening"} {
		t.Run(scenario, func(t *testing.T) {
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, listenErr)
			defer listener.Close()
			go func() {
				for attempt := 0; ; attempt++ {
					accepted, acceptErr := listener.Accept()
					if acceptErr != nil {
						return
					}
					request := make([]byte, len("request"))
					_, readErr := io.ReadFull(accepted, request)
					if readErr == nil && scenario == "proxy-close" && attempt > 0 {
						_, _ = accepted.Write(append([]byte("reply:"), request...))
						_, _ = io.Copy(io.Discard, accepted)
					}
					_ = accepted.Close()
				}
			}()

			ctx, cancel := testutil.GetTestContext(t, 20*time.Second)
			defer cancel()
			// The never-listening scenario needs a short deadline to observe redial expiry.
			redialCtx, redialCancel := context.WithTimeout(ctx, 2*time.Second)
			dial := func(dialCtx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(dialCtx, "tcp", listener.Addr().String())
			}
			first, dialErr := dial(redialCtx)
			require.NoError(t, dialErr)
			connection := newRedialingAdapterConnection(redialCtx, redialCancel, dial, first)
			defer connection.Close()

			written, writeErr := connection.Write([]byte("request"))
			require.NoError(t, writeErr)
			require.Equal(t, len("request"), written)
			response := make([]byte, len("reply:request"))
			_, readErr := io.ReadFull(connection, response)
			if scenario == "never-listening" {
				require.Error(t, readErr)
				return
			}
			require.NoError(t, readErr)
			require.Equal(t, "reply:request", string(response))
		})
	}
}
