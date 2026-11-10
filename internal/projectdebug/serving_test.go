/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	clientrest "k8s.io/client-go/rest"
	clientwebsocket "k8s.io/client-go/transport/websocket"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dap"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// adapterServingExecutor injects echo-adapter behavior only at the process-executor boundary.
type adapterServingExecutor struct {
	*internaltest.TestProcessExecutor
	mu          sync.Mutex
	stops       map[process.Pid_t]func()
	unconfirmed atomic.Bool
}

// Verifies that unread adapter output fails the real debug endpoint at the backlog limit,
// closes a non-reading client's connection, and releases stdio and TCP adapter executions.
func TestProjectDebugServingDisconnectsSlowClient(t *testing.T) {
	for _, mode := range []apiv2.DebugAdapterMode{
		apiv2.DebugAdapterModeStdio, apiv2.DebugAdapterModeTCPConnect, apiv2.DebugAdapterModeTCPCallback,
	} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			apiClient, config, executor := startDebugServingTest(t, ctx)
			command := "stdio"
			if mode == apiv2.DebugAdapterModeTCPConnect {
				command = "listen"
			} else if mode == apiv2.DebugAdapterModeTCPCallback {
				command = "callback"
			}
			project := createDebugServingProject(t, ctx, apiClient, mode, command)
			connection, connectErr := connectDebugServing(ctx, config, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
			defer stopCancellation()
			deadline, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			require.NoError(t, connection.SetWriteDeadline(deadline))
			body, encodeErr := json.Marshal(map[string]string{"output": strings.Repeat("x", 64*1024)})
			require.NoError(t, encodeErr)
			var frame bytes.Buffer
			require.NoError(t, dap.WriteMessage(&frame, dap.Message{
				"seq": json.RawMessage("1"), "type": json.RawMessage(`"event"`),
				"event": json.RawMessage(`"output"`), "body": body,
			}))
			floodDone := make(chan error, 1)
			go func() {
				defer close(floodDone)
				for {
					if writeErr := connection.WriteMessage(websocket.BinaryMessage, frame.Bytes()); writeErr != nil {
						floodDone <- writeErr
						return
					}
				}
			}()
			t.Cleanup(func() { _ = connection.Close(); <-floodDone })
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
				current := &apiv2.Project{}
				if getErr := apiClient.Get(ctx, project.NamespacedName(), current); getErr != nil {
					return false, getErr
				}
				if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 1 {
					return false, nil
				}
				session := current.Status.Debug.Sessions[0]
				if session.Phase != apiv2.ProjectDebugPhaseFailed {
					return false, nil
				}
				require.NotEmpty(t, session.Conditions)
				require.Contains(t, session.Conditions[0].Message, usvc_io.ErrBufferFull.Error())
				return true, nil
			}))
			select {
			case writeErr := <-floodDone:
				require.Error(t, writeErr)
			case <-ctx.Done():
				t.Fatal("buffer overflow left the client connection open")
			}
			executions := executor.FindAll([]string{"test-adapter"}, "", nil)
			require.Len(t, executions, 1)
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
				handle := process.NewHandle(executions[0].PID, executions[0].StartedAt)
				return process.IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
			}))
		})
	}
}

// Verifies that prerequisite waiting outlasts the adapter connection timeout and uses no adapter
// process before readiness, then upgrades and forwards bytes once prerequisites become available.
func TestProjectDebugServingWaitsForPrerequisites(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	// Use a separate resource because adapter configuration is immutable.
	require.NoError(t, apiClient.Delete(ctx, project))
	project.ResourceVersion, project.UID = "", ""
	project.Spec.DebugAdapter.ConnectionTimeout = &metav1.Duration{Duration: 100 * time.Millisecond}
	project.Status = apiv2.ProjectStatus{}
	require.NoError(t, apiClient.Create(ctx, project))
	connectCtx, cancelConnect := context.WithCancel(ctx)
	defer cancelConnect()
	type result struct {
		connection *websocket.Conn
		err        error
	}
	connected := make(chan result, 1)
	go func() {
		connection, connectErr := connectDebugServing(connectCtx, config, project)
		connected <- result{connection, connectErr}
	}()
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		return current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1, nil
	}))
	timer := time.NewTimer(2 * project.Spec.DebugAdapter.GetConnectionTimeout())
	defer timer.Stop()
	select {
	case <-timer.C:
	case early := <-connected:
		if early.connection != nil {
			_ = early.connection.Close()
		}
		t.Fatalf("debug stream completed before readiness: %v", early.err)
	case <-ctx.Done():
		t.Fatal("test timed out before prerequisite check")
	}
	require.Empty(t, executor.FindAll([]string{"test-adapter"}, "", nil))
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	project.Status.Debug.PrerequisitesReady = true
	require.NoError(t, apiClient.Status().Update(ctx, project))
	select {
	case established := <-connected:
		require.NoError(t, established.err)
		exchangeDebugBytes(t, ctx, established.connection)
		closeDebugServing(t, established.connection)
	case <-ctx.Done():
		t.Fatal("ready debug stream did not upgrade")
	}
}

// Verifies that failed TCP setup returns before upgrade and retains an unfinished provider
// fence when cleanup is unconfirmed, while confirmed cleanup permits a subsequent launch.
func TestProjectDebugServingSetupTimeout(t *testing.T) {
	for _, unconfirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("unconfirmed=%t", unconfirmed), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			apiClient, config, executor := startDebugServingTest(t, ctx)
			project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "stall")
			require.NoError(t, apiClient.Delete(ctx, project))
			project.ResourceVersion, project.UID = "", ""
			project.Spec.DebugAdapter.ConnectionTimeout = &metav1.Duration{Duration: 100 * time.Millisecond}
			project.Status = apiv2.ProjectStatus{}
			require.NoError(t, apiClient.Create(ctx, project))
			project.Status.Debug = &apiv2.ProjectDebugStatus{PrerequisitesReady: true}
			require.NoError(t, apiClient.Status().Update(ctx, project))
			executor.unconfirmed.Store(unconfirmed)
			defer executor.unconfirmed.Store(false)
			for attempt := 1; attempt <= 2; attempt++ {
				connectCtx, cancelConnect := context.WithTimeout(ctx, time.Second)
				connection, connectErr := connectDebugServing(connectCtx, config, project)
				cancelConnect()
				if connection != nil {
					require.NoError(t, connection.Close())
				}
				require.Error(t, connectErr)
				expectedExecutions := attempt
				if unconfirmed {
					expectedExecutions = 1
				}
				executions := executor.FindAll([]string{"test-adapter"}, "", nil)
				require.Len(t, executions, expectedExecutions)
				require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
					current := &apiv2.Project{}
					if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
						return false, getErr
					}
					if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != attempt ||
						len(current.Status.Debug.Executions) != expectedExecutions {
						return false, nil
					}
					for _, session := range current.Status.Debug.Sessions {
						if session.Phase != apiv2.ProjectDebugPhaseFailed {
							return false, nil
						}
					}
					for _, observed := range current.Status.Debug.Executions {
						if observed.Phase != apiv2.ProjectDebugPhaseFailed || observed.FinishedAt.IsZero() != unconfirmed {
							return false, nil
						}
					}
					return true, nil
				}))
				for _, execution := range executions {
					handle := process.NewHandle(execution.PID, execution.StartedAt)
					require.True(t, process.IsProcessGoneErr(executor.TestProcessExecutor.CheckProcessRunning(handle)),
						"injected uncertainty must not leak fixture processes")
					if unconfirmed {
						require.NoError(t, executor.CheckProcessRunning(handle))
					}
				}
			}
		})
	}
}

// Verifies that simultaneous TCP-connect requests share one provider, keep sibling sessions usable,
// and a new request after final release launches a new execution rather than reusing a stopped provider.
func TestProjectDebugServingConcurrentProviderAcquisition(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "listen")
	type result struct {
		connection *websocket.Conn
		err        error
	}
	const count = 6
	results := make(chan result, count)
	for range count {
		go func() {
			connection, connectErr := connectDebugServing(ctx, config, project)
			results <- result{connection, connectErr}
		}()
	}
	var connections []*websocket.Conn
	for range count {
		select {
		case connected := <-results:
			require.NoError(t, connected.err)
			connections = append(connections, connected.connection)
			t.Cleanup(func() { _ = connected.connection.Close() })
		case <-ctx.Done():
			t.Fatal("concurrent debug connection did not complete")
		}
	}
	require.Len(t, executor.FindAll([]string{"test-adapter"}, "", nil), 1)
	for _, connection := range connections {
		exchangeDebugBytes(t, ctx, connection)
		closeDebugServing(t, connection)
	}
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		for _, session := range current.Status.Debug.Sessions {
			if session.Phase == apiv2.ProjectDebugPhaseRunning {
				return false, nil
			}
		}
		return true, nil
	}))
	fresh, freshErr := connectDebugServing(ctx, config, project)
	require.NoError(t, freshErr)
	defer fresh.Close()
	require.Len(t, executor.FindAll([]string{"test-adapter"}, "", nil), 2)
	exchangeDebugBytes(t, ctx, fresh)
	closeDebugServing(t, fresh)
}

// Verifies that Project stop terminates active adapter sessions and processes independently of
// application status, without waiting for the client to close its WebSocket.
func TestProjectDebugServingParentStop(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	connection, connectErr := connectDebugServing(ctx, config, project)
	require.NoError(t, connectErr)
	defer connection.Close()
	exchangeDebugBytes(t, ctx, connection)
	require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
	project.Spec.Stop = true
	require.NoError(t, apiClient.Update(ctx, project))
	_, _, readErr := connection.ReadMessage()
	require.Error(t, readErr)
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		if len(current.Status.Debug.Executions) != 1 {
			return false, nil
		}
		return current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseExited, nil
	}))
	executions := executor.FindAll([]string{"test-adapter"}, "", nil)
	require.Len(t, executions, 1)
	require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(process.NewHandle(executions[0].PID, executions[0].StartedAt))))
}

// Verifies that caller cancellation during prerequisite waiting leaves a terminal session observation
// and never launches an adapter after the request has gone away.
func TestProjectDebugServingCancelsPendingSession(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	project.Status.Debug.PrerequisitesReady = false
	require.NoError(t, apiClient.Status().Update(ctx, project))
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	httpClient, clientErr := clientrest.HTTPClientFor(config)
	require.NoError(t, clientErr)
	request, requestErr := http.NewRequestWithContext(requestCtx, http.MethodGet,
		config.Host+"/apis/"+apiv2.GroupVersion.String()+"/namespaces/debug/projects/project/debug", nil)
	require.NoError(t, requestErr)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	result := make(chan error, 1)
	go func() {
		response, connectErr := httpClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		result <- connectErr
	}()
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		return current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1, nil
	}))
	cancelRequest()
	select {
	case cancellationErr := <-result:
		require.Error(t, cancellationErr)
	case <-ctx.Done():
		t.Fatal("cancelled debug request did not return")
	}
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		return current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseFailed, nil
	}))
	require.Empty(t, executor.FindAll([]string{"test-adapter"}, "", nil))
}

// Verifies that unconfigured Projects, stopping Projects,
// and non-upgrade requests fail explicitly without creating adapter executions.
func TestProjectDebugServingAdmission(t *testing.T) {
	for _, scenario := range []string{"unconfigured", "stopping", "not-websocket"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, time.Minute)
			defer cancel()
			apiClient, config, executor := startDebugServingTest(t, ctx)
			project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
			require.NoError(t, apiClient.Delete(ctx, project))
			project.ResourceVersion, project.UID = "", ""
			project.Status = apiv2.ProjectStatus{}
			switch scenario {
			case "unconfigured":
				project.Spec.DebugAdapter = nil
			case "stopping":
				project.Spec.Stop = true
			}
			require.NoError(t, apiClient.Create(ctx, project))
			if scenario == "not-websocket" {
				httpClient, httpClientErr := clientrest.HTTPClientFor(config)
				require.NoError(t, httpClientErr)
				request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet,
					config.Host+"/apis/"+apiv2.GroupVersion.String()+"/namespaces/debug/projects/project/debug", nil)
				require.NoError(t, requestErr)
				response, responseErr := httpClient.Do(request)
				require.NoError(t, responseErr)
				require.Equal(t, http.StatusBadRequest, response.StatusCode)
				require.NoError(t, response.Body.Close())
			} else {
				connection, connectErr := connectDebugServing(ctx, config, project)
				if connection != nil {
					_ = connection.Close()
				}
				require.Error(t, connectErr)
			}
			require.Empty(t, executor.FindAll([]string{"test-adapter"}, "", nil))
		})
	}
}

// Verifies that server-lifetime cancellation closes upgraded sockets and stops adapter processes,
// rather than relying on HTTP shutdown to close hijacked connections.
func TestProjectDebugServingServerShutdown(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	apiClient, config, executor := startDebugServingTest(t, serverCtx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	connection, connectErr := connectDebugServing(ctx, config, project)
	require.NoError(t, connectErr)
	defer connection.Close()
	exchangeDebugBytes(t, ctx, connection)
	cancelServer()
	_, _, readErr := connection.ReadMessage()
	require.Error(t, readErr)
	executions := executor.FindAll([]string{"test-adapter"}, "", nil)
	require.Len(t, executions, 1)
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		return process.IsProcessGoneErr(executor.CheckProcessRunning(process.NewHandle(executions[0].PID, executions[0].StartedAt))), nil
	}))
}
func (executor *adapterServingExecutor) StartProcess(
	ctx context.Context, cmd *exec.Cmd, handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag, creator process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	if len(cmd.Args) > 1 && cmd.Args[1] == "fail" {
		return process.ProcessHandle{}, nil, errors.New("injected adapter launch failure")
	}
	handle, startWait, startErr := executor.TestProcessExecutor.StartProcess(ctx, cmd, handler, flags, creator)
	if startErr != nil {
		return handle, startWait, startErr
	}
	var once sync.Once
	var closeResources func()
	switch cmd.Args[1] {
	case "stdio":
		closeResources = func() {
			_ = cmd.Stdin.(io.Closer).Close()
			_ = cmd.Stdout.(io.Closer).Close()
		}
		go serveTestDebugAdapter(cmd.Stdin, cmd.Stdout, func() {
			_ = executor.StopProcess(context.WithoutCancel(ctx), handle)
		})
	case "callback":
		connection, connectErr := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", cmd.Args[2]))
		if connectErr != nil {
			startWait()
			_ = executor.TestProcessExecutor.StopProcess(ctx, handle)
			return process.ProcessHandle{}, nil, connectErr
		}
		closeResources = func() { _ = connection.Close() }
		go serveTestDebugAdapter(connection, connection, func() {
			_ = executor.StopProcess(context.WithoutCancel(ctx), handle)
		})
	case "listen":
		listener, listenErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", cmd.Args[2]))
		if listenErr != nil {
			startWait()
			_ = executor.TestProcessExecutor.StopProcess(ctx, handle)
			return process.ProcessHandle{}, nil, listenErr
		}
		var connectionsMu sync.Mutex
		var connections []net.Conn
		closed := false
		closeResources = func() {
			_ = listener.Close()
			connectionsMu.Lock()
			defer connectionsMu.Unlock()
			closed = true
			for _, connection := range connections {
				_ = connection.Close()
			}
		}
		go func() {
			for {
				connection, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				connectionsMu.Lock()
				if closed {
					_ = connection.Close()
				} else {
					connections = append(connections, connection)
					go serveTestDebugAdapter(connection, connection, func() { _ = connection.Close() })
				}
				connectionsMu.Unlock()
			}
		}()
	default:
		closeResources = func() {}
	}
	executor.mu.Lock()
	executor.stops[handle.Pid] = func() { once.Do(closeResources) }
	executor.mu.Unlock()
	context.AfterFunc(ctx, func() { _ = executor.StopProcess(context.WithoutCancel(ctx), handle) })
	return handle, startWait, nil
}

func serveTestDebugAdapter(input io.Reader, output io.Writer, terminate func()) {
	reader := bufio.NewReader(input)
	for {
		message, readErr := dap.ReadMessage(reader)
		if readErr != nil {
			return
		}
		var command string
		_ = json.Unmarshal(message["command"], &command)
		if command == "disconnect" {
			response := dap.Message{
				"seq": message["seq"], "type": json.RawMessage(`"response"`),
				"request_seq": message["seq"], "command": message["command"], "success": json.RawMessage("true"),
			}
			_ = dap.WriteMessage(output, response)
			terminate()
			return
		}
		if writeErr := dap.WriteMessage(output, message); writeErr != nil {
			return
		}
	}
}

func (executor *adapterServingExecutor) StopProcess(ctx context.Context, handle process.ProcessHandle, options ...process.ProcessStopOption) error {
	executor.mu.Lock()
	stop := executor.stops[handle.Pid]
	executor.mu.Unlock()
	if stop != nil {
		stop()
	}
	stopErr := executor.TestProcessExecutor.StopProcess(ctx, handle, options...)
	if executor.unconfirmed.Load() {
		return errors.Join(stopErr, errors.New("injected adapter cleanup uncertainty"))
	}
	return stopErr
}

func (executor *adapterServingExecutor) CheckProcessRunning(handle process.ProcessHandle) error {
	if executor.unconfirmed.Load() {
		return nil
	}
	return executor.TestProcessExecutor.CheckProcessRunning(handle)
}

func startDebugServingTest(t *testing.T, ctx context.Context, containerRuntime ...func(context.Context, string) (containers.ExecContainers, error)) (ctrlclient.WithWatch, *clientrest.Config, *adapterServingExecutor) {
	t.Helper()
	// Exit callbacks must remain deliverable while the server is performing shutdown cleanup.
	executor := &adapterServingExecutor{TestProcessExecutor: internaltest.NewTestProcessExecutor(context.WithoutCancel(ctx)), stops: make(map[process.Pid_t]func())}
	var clientConfig *clientrest.Config
	apiClient, _ := startProjectDebugStatusServer(t, ctx, func(config *tiltapiserver.Config, writer *projectDebugStatusWriter) {
		clientConfig = clientrest.CopyConfig(config.GenericConfig.LoopbackClientConfig)
		gvr := (&apiv2.Project{}).GetGroupVersionResource()
		parentProvider := config.ExtraConfig.APIs[gvr]
		namespaceProvider := config.ExtraConfig.APIs[(&apiv2.Namespace{}).GetGroupVersionResource()]
		gvr.Resource += "/" + apiv2.DebugSubresourceName
		config.ExtraConfig.APIs[gvr] = func(scheme *runtime.Scheme, getter generic.RESTOptionsGetter) (rest.Storage, error) {
			parent, parentErr := parentProvider(scheme, getter)
			if parentErr != nil {
				return nil, parentErr
			}
			namespaces, namespaceErr := namespaceProvider(scheme, getter)
			if namespaceErr != nil {
				return nil, namespaceErr
			}
			storage := newProjectDebugStorage(ctx, parent.(rest.StandardStorage), namespaces.(rest.Getter), writer, executor, logr.Discard())
			if len(containerRuntime) != 0 {
				storage.runtime.findContainerRuntime = containerRuntime[0]
			}
			return storage, nil
		}
	})
	return apiClient, clientConfig, executor
}

func createDebugServingProject(t *testing.T, ctx context.Context, apiClient ctrlclient.Client, mode apiv2.DebugAdapterMode, command string, placement ...apiv2.DebugAdapterPlacement) *apiv2.Project {
	t.Helper()
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "debug"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	namespace.Status.Phase = apiv2.NamespacePhaseActive
	require.NoError(t, apiClient.Status().Update(ctx, namespace))
	project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "application"}},
			DebugAdapter: &apiv2.DebugAdapterConfig{Args: []string{"test-adapter", command, "{{port}}"}, Mode: mode}}}
	if len(placement) != 0 {
		project.Spec.Process = nil
		project.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "image"}}
		project.Spec.DebugAdapter.Placement = placement[0]
	}
	require.NoError(t, apiClient.Create(ctx, project))
	project.Status.Phase = apiv2.ProjectPhaseReady
	project.Status.Debug = &apiv2.ProjectDebugStatus{PrerequisitesReady: true}
	if project.Spec.Container != nil {
		project.Status.ContainerID, project.Status.ContainerRuntime = "container-id", "test-runtime"
	}
	require.NoError(t, apiClient.Status().Update(ctx, project))
	return project
}

func connectDebugServing(ctx context.Context, config *clientrest.Config, project *apiv2.Project) (*websocket.Conn, error) {
	transport, holder, transportErr := clientwebsocket.RoundTripperFor(config)
	if transportErr != nil {
		return nil, transportErr
	}
	location := config.Host + "/apis/" + apiv2.GroupVersion.String() + "/namespaces/" + project.Namespace + "/projects/" + project.Name + "/debug"
	request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if requestErr != nil {
		return nil, requestErr
	}
	return clientwebsocket.Negotiate(transport, holder, request, "dap")
}

func exchangeDebugBytes(t *testing.T, ctx context.Context, connection *websocket.Conn) {
	t.Helper()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, connection.SetReadDeadline(deadline))
	body := `{"seq":1,"type":"request","command":"initialize","arguments":{}}`
	payload := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	// DAP framing is independent of WebSocket message boundaries.
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(payload[:10])))
	require.NoError(t, connection.WriteMessage(websocket.BinaryMessage, []byte(payload[10:])))
	var received []byte
	for len(received) < len(payload) {
		_, chunk, readErr := connection.ReadMessage()
		require.NoError(t, readErr)
		received = append(received, chunk...)
	}
	actual, decodeErr := dap.ReadMessage(bufio.NewReader(strings.NewReader(string(received))))
	require.NoError(t, decodeErr)
	require.JSONEq(t, `"request"`, string(actual["type"]))
	require.JSONEq(t, `"initialize"`, string(actual["command"]))
	require.JSONEq(t, `{}`, string(actual["arguments"]))
}

func closeDebugServing(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	require.NoError(t, connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")))
	require.NoError(t, connection.Close())
}

// Verifies that adapter completion drains its final DAP bytes before ending the stream,
// records the adapter exit code, and leaves independently observed application status unchanged.
func TestProjectDebugServingAdapterCompletion(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio")
	connection, connectErr := connectDebugServing(ctx, config, project)
	require.NoError(t, connectErr)
	defer connection.Close()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, connection.SetReadDeadline(deadline))
	executions := executor.FindAll([]string{"test-adapter"}, "", nil)
	require.Len(t, executions, 1)
	body := `{"seq":2,"type":"event","event":"terminated"}`
	payload := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	_, writeErr := io.WriteString(executions[0].Cmd.Stdout, payload)
	require.NoError(t, writeErr)
	executor.SimulateProcessExit(t, executions[0].PID, 7)
	_, received, readErr := connection.ReadMessage()
	require.NoError(t, readErr)
	actual, decodeErr := dap.ReadMessage(bufio.NewReader(strings.NewReader(string(received))))
	require.NoError(t, decodeErr)
	require.JSONEq(t, `"terminated"`, string(actual["event"]))
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		if current.Status.Debug == nil || len(current.Status.Debug.Executions) != 1 || len(current.Status.Debug.Sessions) != 1 {
			return false, nil
		}
		if current.Status.Debug.Sessions[0].Phase != apiv2.ProjectDebugPhaseExited {
			return false, nil
		}
		require.Equal(t, apiv2.ProjectPhaseReady, current.Status.Phase)
		require.NotNil(t, current.Status.Debug.Executions[0].ExitCode)
		require.Equal(t, int32(7), *current.Status.Debug.Executions[0].ExitCode)
		return true, nil
	}))
}

// Verifies that unconfirmed provider cleanup is reported without a false finish time,
// and fences a new request instead of launching a duplicate provider.
func TestProjectDebugServingFencesUnconfirmedCleanup(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "listen")
	connection, connectErr := connectDebugServing(ctx, config, project)
	require.NoError(t, connectErr)
	exchangeDebugBytes(t, ctx, connection)
	executor.unconfirmed.Store(true)
	defer executor.unconfirmed.Store(false)
	closeDebugServing(t, connection)
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		execution := current.Status.Debug.Executions[0]
		return execution.Phase == apiv2.ProjectDebugPhaseFailed && execution.FinishedAt.IsZero() &&
			current.Status.Debug.Sessions[0].Phase == apiv2.ProjectDebugPhaseFailed, nil
	}))
	retryCtx, cancelRetry := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelRetry()
	retried, retryErr := connectDebugServing(retryCtx, config, project)
	if retried != nil {
		_ = retried.Close()
	}
	require.Error(t, retryErr)
	require.Len(t, executor.FindAll([]string{"test-adapter"}, "", nil), 1)
}

// Verifies that closing one shared-provider TCP connection bounds final output to a slow client
// without stopping the provider or disrupting a sibling session.
func TestProjectDebugServingSharedConnectionEOFBoundsDrain(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	apiClient, config, executor := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "listen")
	first, firstErr := connectDebugServing(ctx, config, project)
	require.NoError(t, firstErr)
	defer first.Close()
	second, secondErr := connectDebugServing(ctx, config, project)
	require.NoError(t, secondErr)
	defer second.Close()
	exchangeDebugBytes(t, ctx, first)
	exchangeDebugBytes(t, ctx, second)
	secured, securedOK := first.UnderlyingConn().(*tls.Conn)
	require.True(t, securedOK)
	socket, socketOK := secured.NetConn().(*net.TCPConn)
	require.True(t, socketOK)
	require.NoError(t, socket.SetReadBuffer(1024))
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, first.SetWriteDeadline(deadline))
	var frame bytes.Buffer
	payload, encodeErr := json.Marshal(strings.Repeat("x", 7*1024*1024))
	require.NoError(t, encodeErr)
	require.NoError(t, dap.WriteMessage(&frame, dap.Message{
		"seq": json.RawMessage("71"), "type": json.RawMessage(`"request"`),
		"command": json.RawMessage(`"customExtension"`), "arguments": payload,
	}))
	require.NoError(t, first.WriteMessage(websocket.BinaryMessage, frame.Bytes()))
	frame.Reset()
	require.NoError(t, dap.WriteMessage(&frame, dap.Message{
		"seq": json.RawMessage("72"), "type": json.RawMessage(`"request"`),
		"command": json.RawMessage(`"disconnect"`), "arguments": json.RawMessage(`{}`),
	}))
	require.NoError(t, first.WriteMessage(websocket.BinaryMessage, frame.Bytes()))
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 2 || len(current.Status.Debug.Executions) != 1 {
			return false, nil
		}
		var failed, running int
		for _, session := range current.Status.Debug.Sessions {
			if session.Phase == apiv2.ProjectDebugPhaseFailed {
				require.NotEmpty(t, session.Conditions)
				require.Contains(t, session.Conditions[0].Message, "output drain timed out")
				failed++
			} else if session.Phase == apiv2.ProjectDebugPhaseRunning {
				running++
			}
		}
		return failed == 1 && running == 1 && current.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseRunning, nil
	}))
	exchangeDebugBytes(t, ctx, second)
	executions := executor.FindAll([]string{"test-adapter"}, "", nil)
	require.Len(t, executions, 1)
	handle := process.NewHandle(executions[0].PID, executions[0].StartedAt)
	require.NoError(t, executor.CheckProcessRunning(handle))
	closeDebugServing(t, second)
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		return process.IsProcessGoneErr(executor.CheckProcessRunning(handle)), nil
	}))
}

// Verifies that the actual debug subresource launches per-stream stdio/callback adapters or one
// shared TCP-connect provider, relays unmodified DAP bytes, and cleans the provider only on final release.
func TestProjectDebugServingTransportsAndSharing(t *testing.T) {
	for _, testCase := range []struct {
		mode       apiv2.DebugAdapterMode
		command    string
		executions int
	}{
		{apiv2.DebugAdapterModeStdio, "stdio", 2},
		{apiv2.DebugAdapterModeTCPCallback, "callback", 2},
		{apiv2.DebugAdapterModeTCPConnect, "listen", 1},
	} {
		t.Run(string(testCase.mode), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, time.Minute)
			defer cancel()
			apiClient, config, executor := startDebugServingTest(t, ctx)
			project := createDebugServingProject(t, ctx, apiClient, testCase.mode, testCase.command)
			require.Empty(t, executor.FindAll([]string{"test-adapter"}, "", nil))
			first, firstErr := connectDebugServing(ctx, config, project)
			require.NoError(t, firstErr)
			defer first.Close()
			second, secondErr := connectDebugServing(ctx, config, project)
			require.NoError(t, secondErr)
			defer second.Close()
			require.Len(t, executor.FindAll([]string{"test-adapter"}, "", nil), testCase.executions)
			exchangeDebugBytes(t, ctx, first)
			exchangeDebugBytes(t, ctx, second)
			closeDebugServing(t, first)
			exchangeDebugBytes(t, ctx, second)
			closeDebugServing(t, second)
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
				current := &apiv2.Project{}
				if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
					return false, getErr
				}
				if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 2 || len(current.Status.Debug.Executions) != testCase.executions {
					return false, nil
				}
				for _, session := range current.Status.Debug.Sessions {
					if session.Phase != apiv2.ProjectDebugPhaseFailed {
						return false, nil
					}
				}
				for _, execution := range current.Status.Debug.Executions {
					if execution.Phase != apiv2.ProjectDebugPhaseExited {
						return false, nil
					}
				}
				require.Equal(t, apiv2.ProjectPhaseReady, current.Status.Phase)
				require.True(t, current.Status.Debug.PrerequisitesReady)
				return true, nil
			}))
		})
	}
}

// Verifies that a launch failure returns an API error before upgrade and records one failed
// execution/session without fabricating a process identity or changing the application's phase.
func TestProjectDebugServingLaunchFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	apiClient, config, _ := startDebugServingTest(t, ctx)
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "fail")
	connection, connectErr := connectDebugServing(ctx, config, project)
	require.Error(t, connectErr)
	require.Nil(t, connection)
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}

		if current.Status.Debug == nil || len(current.Status.Debug.Sessions) != 1 || len(current.Status.Debug.Executions) != 1 {
			return false, nil
		}
		if current.Status.Debug.Sessions[0].Phase != apiv2.ProjectDebugPhaseFailed {
			return false, nil
		}
		require.Equal(t, apiv2.ProjectDebugPhaseFailed, current.Status.Debug.Executions[0].Phase)
		require.Nil(t, current.Status.Debug.Executions[0].PID)
		require.Equal(t, apiv2.ProjectPhaseReady, current.Status.Phase)
		return true, nil
	}))
}

// Verifies that an outstanding upgrade waits for Project to publish its TCP binding without
// consuming setup timeout or mistaking initial endpoint publication for a replacement.
func TestProjectDebugServingWaitsForContainerTCPEndpoint(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	defer listener.Close()
	completion := make(chan int32, 1)
	containerExec := &containerAdapterExecutor{tcpListener: listener, tcpComplete: completion}
	apiClient, config, _ := startDebugServingTest(t, ctx,
		func(context.Context, string) (containers.ExecContainers, error) { return containerExec, nil })
	project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "listen", apiv2.DebugAdapterPlacementContainer)
	type result struct {
		connection *websocket.Conn
		err        error
	}
	connected := make(chan result, 1)
	selected := project.DeepCopy()
	go func() {
		connection, connectErr := connectDebugServing(ctx, config, selected)
		connected <- result{connection, connectErr}
	}()
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		current := &apiv2.Project{}
		if getErr := apiClient.Get(pollCtx, project.NamespacedName(), current); getErr != nil {
			return false, getErr
		}
		return current.Status.Debug != nil && len(current.Status.Debug.Sessions) == 1, nil
	}))
	containerExec.mu.Lock()
	count := len(containerExec.options)
	containerExec.mu.Unlock()
	require.Zero(t, count, "endpoint preparation must precede adapter launch")
	require.NoError(t, apiClient.Status().Patch(ctx, project, ctrlclient.RawPatch(types.MergePatchType,
		[]byte(fmt.Sprintf(`{"status":{"debugAdapterPort":4711,"debugAdapterAddress":%q}}`, listener.Addr().String())))))
	select {
	case ready := <-connected:
		require.NoError(t, ready.err)
		require.NotNil(t, ready.connection)
		defer ready.connection.Close()
		exchangeDebugBytes(t, ctx, ready.connection)
		completion <- 0
	case <-ctx.Done():
		t.Fatal("Project endpoint publication did not unblock adapter setup")
	}
}

type containerAdapterExecutor struct {
	mu           sync.Mutex
	options      []containers.ExecContainerOptions
	fail         bool
	noCompletion bool
	tcpListener  net.Listener
	tcpComplete  <-chan int32
	run          func(context.Context, containers.ExecContainerOptions) (<-chan int32, error)
}

// Verifies that container TCP streams share one provider, keep sibling connections independent,
// use the prepared host endpoint and internal port, and fence uncertain final cleanup or failed setup.
func TestProjectDebugServingContainerTCPProvider(t *testing.T) {
	for _, scenario := range []string{"shared", "confirmed-exit", "failed-connect", "changed-endpoint"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, time.Minute)
			defer cancel()
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, listenErr)
			defer listener.Close()
			completion := make(chan int32, 1)
			containerExec := &containerAdapterExecutor{tcpListener: listener, tcpComplete: completion}
			apiClient, config, hostExecutor := startDebugServingTest(t, ctx,
				func(context.Context, string) (containers.ExecContainers, error) { return containerExec, nil })
			project := createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeTCPConnect, "listen", apiv2.DebugAdapterPlacementContainer)
			require.NoError(t, apiClient.Status().Patch(ctx, project, ctrlclient.RawPatch(types.MergePatchType,
				[]byte(fmt.Sprintf(`{"status":{"debugAdapterPort":4711,"debugAdapterAddress":%q}}`, listener.Addr().String())))))
			if scenario == "failed-connect" {
				completion <- process.UnknownExitCode
				require.NoError(t, listener.Close())
				connection, connectErr := connectDebugServing(ctx, config, project)
				if connection != nil {
					_ = connection.Close()
				}
				require.Error(t, connectErr)
			} else {
				var connections []*websocket.Conn
				results := make(chan *websocket.Conn, 2)
				errorsCh := make(chan error, 2)
				for range 2 {
					go func() {
						connection, connectErr := connectDebugServing(ctx, config, project)
						results <- connection
						errorsCh <- connectErr
					}()
				}
				for range 2 {
					connection := <-results
					require.NoError(t, <-errorsCh)
					require.NotNil(t, connection)
					defer connection.Close()
					connections = append(connections, connection)
					exchangeDebugBytes(t, ctx, connection)
				}
				closeDebugServing(t, connections[0])
				exchangeDebugBytes(t, ctx, connections[1])
				if scenario == "confirmed-exit" {
					completion <- 0
					require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
						if getErr := apiClient.Get(pollCtx, project.NamespacedName(), project); getErr != nil {
							return false, getErr
						}
						return project.Status.Debug.Executions[0].Phase == apiv2.ProjectDebugPhaseExited, nil
					}))
				} else if scenario == "changed-endpoint" {
					require.NoError(t, apiClient.Status().Patch(ctx, project, ctrlclient.RawPatch(types.MergePatchType,
						[]byte(`{"status":{"debugAdapterAddress":"127.0.0.1:1"}}`))))
				}
				_ = connections[1].Close()
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
				if getErr := apiClient.Get(pollCtx, project.NamespacedName(), project); getErr != nil {
					return false, getErr
				}
				if len(project.Status.Debug.Sessions) == 0 {
					return false, nil
				}
				for _, session := range project.Status.Debug.Sessions {
					if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
						return false, nil
					}
				}
				return true, nil
			}))
			containerExec.mu.Lock()
			invocations := append([]containers.ExecContainerOptions(nil), containerExec.options...)
			containerExec.mu.Unlock()
			require.Len(t, invocations, 1)
			require.Equal(t, "container-id", invocations[0].Container)
			require.Equal(t, []string{"listen", "4711"}, invocations[0].Args)
			require.Nil(t, invocations[0].StdInStream)
			require.Empty(t, hostExecutor.FindAll([]string{"test-adapter"}, "", nil))
			require.Len(t, project.Status.Debug.Executions, 1)
			if scenario == "confirmed-exit" {
				restartedListener, restartErr := net.Listen("tcp", listener.Addr().String())
				require.NoError(t, restartErr)
				defer restartedListener.Close()
				nextCompletion := make(chan int32, 1)
				containerExec.tcpListener, containerExec.tcpComplete = restartedListener, nextCompletion
				restarted, reconnectErr := connectDebugServing(ctx, config, project)
				require.NoError(t, reconnectErr)
				defer restarted.Close()
				exchangeDebugBytes(t, ctx, restarted)
				containerExec.mu.Lock()
				restartedCount := len(containerExec.options)
				containerExec.mu.Unlock()
				require.Equal(t, 2, restartedCount, "confirmed provider exit permits a new launch")
				nextCompletion <- 0
			} else {
				require.Equal(t, apiv2.ProjectDebugPhaseFailed, project.Status.Debug.Executions[0].Phase)
				retryCtx, retryCancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer retryCancel()
				retried, retryErr := connectDebugServing(retryCtx, config, project)
				if retried != nil {
					_ = retried.Close()
				}
				require.Error(t, retryErr)
				containerExec.mu.Lock()
				count := len(containerExec.options)
				containerExec.mu.Unlock()
				require.Equal(t, 1, count, "uncertain provider cleanup must fence new launches")
			}
		})
	}
}

func (executor *containerAdapterExecutor) ExecContainer(ctx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
	executor.mu.Lock()
	executor.options = append(executor.options, options)
	executor.mu.Unlock()
	if executor.fail {
		return nil, errors.New("injected container adapter launch failure")
	}
	if executor.noCompletion {
		return nil, nil
	}
	if executor.run != nil {
		return executor.run(ctx, options)
	}
	exits := make(chan int32, 1)
	if executor.tcpListener != nil {
		listener := executor.tcpListener
		var connectionsMu sync.Mutex
		var connections []net.Conn
		stop := func() {
			_ = listener.Close()
			connectionsMu.Lock()
			defer connectionsMu.Unlock()
			for _, connection := range connections {
				_ = connection.Close()
			}
		}
		go func() {
			for {
				connection, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				connectionsMu.Lock()
				if ctx.Err() != nil {
					_ = connection.Close()
				} else {
					connections = append(connections, connection)
					go serveTestDebugAdapter(connection, connection, func() { _ = connection.Close() })
				}
				connectionsMu.Unlock()
			}
		}()
		go func() {
			defer stop()
			select {
			case code, open := <-executor.tcpComplete:
				if open {
					exits <- code
				}
			case <-ctx.Done():
				exits <- process.UnknownExitCode
			}
			close(exits)
		}()
		return exits, nil
	}
	go func() {
		code := int32(process.UnknownExitCode)
		serveTestDebugAdapter(options.StdInStream, options.StdOutStream, func() { code = 0 })
		exits <- code
		close(exits)
	}()
	return exits, nil
}

// Verifies that container adapters execute through the selected runtime against a pinned container,
// keep host handles separate, preserve host placement and report uncertain cancellation explicitly.
func TestProjectDebugServingContainerAdapters(t *testing.T) {
	for _, scenario := range []string{"stdio", "host", "disconnect", "replace-live", "replace-before-exec", "lookup-failure", "setup-timeout", "start-failure", "no-completion"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, time.Minute)
			defer cancel()
			containerExec := &containerAdapterExecutor{fail: scenario == "start-failure", noCompletion: scenario == "no-completion"}
			var apiClient ctrlclient.WithWatch
			var project *apiv2.Project
			lookup := func(lookupCtx context.Context, name string) (containers.ExecContainers, error) {
				if name != "test-runtime" {
					return nil, fmt.Errorf("wrong selected runtime %q", name)
				}
				if scenario == "lookup-failure" {
					return nil, errors.New("injected runtime lookup failure")
				}
				if scenario == "setup-timeout" {
					<-lookupCtx.Done()
					return nil, lookupCtx.Err()
				}
				if scenario == "replace-before-exec" {
					if patchErr := apiClient.Status().Patch(lookupCtx, project, ctrlclient.RawPatch(types.MergePatchType,
						[]byte(`{"status":{"containerID":"replacement"}}`))); patchErr != nil {
						return nil, patchErr
					}
					<-lookupCtx.Done()
					require.ErrorContains(t, context.Cause(lookupCtx), "prepared container identity changed")
					return nil, lookupCtx.Err()
				}
				return containerExec, nil
			}
			var config *clientrest.Config
			var hostExecutor *adapterServingExecutor
			apiClient, config, hostExecutor = startDebugServingTest(t, ctx, lookup)
			placement := apiv2.DebugAdapterPlacementContainer
			if scenario == "host" {
				placement = apiv2.DebugAdapterPlacementHost
			}
			project = createDebugServingProject(t, ctx, apiClient, apiv2.DebugAdapterModeStdio, "stdio", placement)
			connection, connectErr := connectDebugServing(ctx, config, project)
			if scenario == "lookup-failure" || scenario == "setup-timeout" || scenario == "replace-before-exec" || scenario == "start-failure" || scenario == "no-completion" {
				if connection != nil {
					_ = connection.Close()
				}
				require.Error(t, connectErr)
				require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
				require.Nil(t, project.Status.Debug.Executions[0].PID)
				require.Equal(t, apiv2.ProjectDebugPhaseFailed, project.Status.Debug.Executions[0].Phase)
				if scenario == "replace-before-exec" || scenario == "lookup-failure" {
					require.Empty(t, containerExec.options)
				}
				return
			}
			require.NoError(t, connectErr)
			defer connection.Close()
			exchangeDebugBytes(t, ctx, connection)
			require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
			execution := project.Status.Debug.Executions[0]
			if scenario == "host" {
				require.NotNil(t, execution.PID)
				require.Empty(t, containerExec.options)
				closeDebugServing(t, connection)
				return
			}
			require.Nil(t, execution.PID)
			require.True(t, execution.IdentityTimestamp.IsZero())
			encodedExecution, encodeErr := json.Marshal(execution)
			require.NoError(t, encodeErr)
			require.NotContains(t, string(encodedExecution), "containerID")
			require.NotContains(t, string(encodedExecution), "containerRuntime")
			require.Equal(t, "container-id", project.Status.ContainerID)
			require.Equal(t, "test-runtime", project.Status.ContainerRuntime)
			require.Empty(t, hostExecutor.FindAll([]string{"test-adapter"}, "", nil))
			containerExec.mu.Lock()
			options := containerExec.options[0]
			containerExec.mu.Unlock()
			require.Equal(t, "container-id", options.Container)
			require.Equal(t, "test-adapter", options.Command)
			require.Equal(t, []string{"stdio", "{{port}}"}, options.Args)
			require.Empty(t, options.Env, "container adapters must not inherit host environment")
			require.NoError(t, dap.WriteMessage(options.StdOutStream, dap.Message{
				"seq": json.RawMessage("50"), "type": json.RawMessage(`"event"`), "event": json.RawMessage(`"process"`),
				"body": json.RawMessage(`{"name":"container-target","systemProcessId":1,"isLocalProcess":true}`),
			}))
			_, _, eventErr := connection.ReadMessage()
			require.NoError(t, eventErr)
			require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
			target := project.Status.Debug.Sessions[0].Processes[0]
			require.Equal(t, "container-id", target.ContainerID)
			require.True(t, target.IdentityTimestamp.IsZero())
			require.NotEmpty(t, target.TrackingError)
			if scenario == "stdio" {
				project.Spec.Stop = true
				require.NoError(t, apiClient.Update(ctx, project))
			} else if scenario == "replace-live" {
				require.NoError(t, apiClient.Status().Patch(ctx, project, ctrlclient.RawPatch(types.MergePatchType,
					[]byte(`{"status":{"containerID":"replacement"}}`))))
			} else {
				closeDebugServing(t, connection)
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
				if getErr := apiClient.Get(pollCtx, project.NamespacedName(), project); getErr != nil {
					return false, getErr
				}
				phase := project.Status.Debug.Sessions[0].Phase
				return phase == apiv2.ProjectDebugPhaseExited || phase == apiv2.ProjectDebugPhaseFailed, nil
			}))
			completed := project.Status.Debug.Executions[0]
			if scenario == "stdio" {
				require.Equal(t, apiv2.ProjectDebugPhaseExited, completed.Phase)
				require.NotNil(t, completed.ExitCode)
			} else {
				require.Equal(t, apiv2.ProjectDebugPhaseFailed, completed.Phase)
				require.True(t, completed.FinishedAt.IsZero())
				require.Equal(t, "ContainerAdapterCleanupUnconfirmed", completed.Conditions[0].Reason)
			}
			containerExec.mu.Lock()
			invocations := append([]containers.ExecContainerOptions(nil), containerExec.options...)
			containerExec.mu.Unlock()
			require.Len(t, invocations, 1)
			require.Equal(t, "container-id", invocations[0].Container, "an execution must never retarget the replacement")
		})
	}
}
