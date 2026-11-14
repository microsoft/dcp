/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

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
