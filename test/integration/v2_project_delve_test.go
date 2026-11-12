/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type projectDelveClient struct {
	connection *websocket.Conn
	reader     *bufio.Reader
	pending    []dap.Message
	sequence   int64
}

type projectDelveStream struct {
	connection *websocket.Conn
	reader     io.Reader
}

func (stream *projectDelveStream) Read(buffer []byte) (int, error) {
	for {
		if stream.reader == nil {
			_, reader, readErr := stream.connection.NextReader()
			if readErr != nil {
				return 0, readErr
			}
			stream.reader = reader
		}
		count, readErr := stream.reader.Read(buffer)
		if readErr == io.EOF {
			stream.reader = nil
			if count == 0 {
				continue
			}
			readErr = nil
		}
		return count, readErr
	}
}

func (client *projectDelveClient) request(t *testing.T, command string, arguments any) int64 {
	t.Helper()
	client.sequence++
	encodedArguments, encodeErr := json.Marshal(arguments)
	require.NoError(t, encodeErr)
	encodedCommand, commandErr := json.Marshal(command)
	require.NoError(t, commandErr)
	var frame bytes.Buffer
	require.NoError(t, dap.WriteMessage(&frame, dap.Message{
		"seq": json.RawMessage(fmt.Sprint(client.sequence)), "type": json.RawMessage(`"request"`),
		"command": encodedCommand, "arguments": encodedArguments,
	}))
	require.NoError(t, client.connection.WriteMessage(websocket.BinaryMessage, frame.Bytes()))
	return client.sequence
}

func (client *projectDelveClient) receive(matches func(dap.Message) bool) (dap.Message, error) {
	for index, message := range client.pending {
		if matches(message) {
			client.pending = append(client.pending[:index], client.pending[index+1:]...)
			return message, nil
		}
	}
	for {
		message, readErr := dap.ReadMessage(client.reader)
		if readErr != nil {
			return nil, readErr
		}
		if matches(message) {
			return message, nil
		}
		if len(client.pending) >= 256 {
			return nil, fmt.Errorf("too many unconsumed Delve test messages")
		}
		client.pending = append(client.pending, message)
	}
}

func (client *projectDelveClient) response(t *testing.T, request int64, body any) {
	t.Helper()
	message, responseErr := client.receive(func(message dap.Message) bool {
		return string(message["type"]) == `"response"` && string(message["request_seq"]) == fmt.Sprint(request)
	})
	require.NoError(t, responseErr)
	require.JSONEq(t, "true", string(message["success"]), "DAP request %d failed: %s", request, message["message"])
	if body != nil {
		require.NoError(t, json.Unmarshal(message["body"], body))
	}
}

func (client *projectDelveClient) event(t *testing.T, name string, body any) {
	t.Helper()
	message, eventErr := client.receive(func(message dap.Message) bool {
		return string(message["type"]) == `"event"` && string(message["event"]) == `"`+name+`"`
	})
	require.NoError(t, eventErr)
	if body != nil {
		require.NoError(t, json.Unmarshal(message["body"], body))
	}
}

// Verifies that the Delve scenario client retains interleaved events and responses without assuming
// launch-response ordering, and reads multiple DAP frames from one byte stream.
func TestProjectDelveClientPreservesInterleavedMessages(t *testing.T) {
	t.Parallel()
	var wire bytes.Buffer
	for _, payload := range []string{
		`{"seq":1,"type":"event","event":"initialized"}`,
		`{"seq":2,"type":"response","command":"launch","request_seq":2,"success":true}`,
		`{"seq":3,"type":"event","event":"stopped","body":{"reason":"breakpoint","threadId":1}}`,
		`{"seq":4,"type":"response","command":"configurationDone","request_seq":4,"success":true}`,
	} {
		var message dap.Message
		require.NoError(t, json.Unmarshal([]byte(payload), &message))
		require.NoError(t, dap.WriteMessage(&wire, message))
	}
	client := &projectDelveClient{reader: bufio.NewReader(&wire)}
	client.response(t, 4, nil)
	client.event(t, "initialized", nil)
	client.response(t, 2, nil)
	var stopped struct{ Reason string }
	client.event(t, "stopped", &stopped)
	require.Equal(t, "breakpoint", stopped.Reason)
	require.Empty(t, client.pending)
}

// Verifies that the Delve scenario client bounds retained unmatched messages and propagates EOF.
func TestProjectDelveClientBoundsPendingMessages(t *testing.T) {
	t.Parallel()
	var wire bytes.Buffer
	for sequence := 1; sequence <= 257; sequence++ {
		require.NoError(t, dap.WriteMessage(&wire, dap.Message{
			"seq": json.RawMessage(fmt.Sprint(sequence)), "type": json.RawMessage(`"event"`),
			"event": json.RawMessage(`"output"`), "body": json.RawMessage(`{"output":"unexpected"}`),
		}))
	}
	client := &projectDelveClient{reader: bufio.NewReader(&wire)}
	_, overflowErr := client.receive(func(dap.Message) bool { return false })
	require.ErrorContains(t, overflowErr, "too many unconsumed")
	require.Len(t, client.pending, 256)
	empty := &projectDelveClient{reader: bufio.NewReader(strings.NewReader(""))}
	_, emptyErr := empty.receive(func(dap.Message) bool { return false })
	require.ErrorIs(t, emptyErr, io.EOF)
}

// Verifies in opt-in CI that a real Delve adapter runs through the production Project DAP endpoint:
// breakpoint, stack/variable inspection, continuation and cleanup work without a TTY or V1 IDE protocol.
func TestV2ProjectDelveEndToEnd(t *testing.T) {
	if !osutil.EnvVarSwitchEnabled("DCP_TEST_ENABLE_DELVE") {
		t.Skip("real Delve E2E requires DCP_TEST_ENABLE_DELVE from the test entry point")
	}
	t.Parallel()
	for _, ending := range []string{"continue", "delete-at-breakpoint"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			source, sourceErr := filepath.Abs(filepath.Join("..", "projectdebug", "main.go"))
			require.NoError(t, sourceErr)
			sourceBytes, readErr := os.ReadFile(source)
			require.NoError(t, readErr)
			breakpoint := 0
			for index, line := range strings.Split(string(sourceBytes), "\n") {
				if strings.Contains(line, "// dap-breakpoint") {
					require.Zero(t, breakpoint, "breakpoint marker must be unique")
					breakpoint = index + 1
				}
			}
			require.Positive(t, breakpoint)
			binary, binaryErr := internaltest.GetTestToolPath("projectdebug")
			require.NoError(t, binaryErr, "build Project debug target with make test-prereqs first")
			executor := process.NewOSExecutor(logr.Discard())
			t.Cleanup(executor.Dispose)
			resolve := exec.Command("go", "tool", "-n", "github.com/go-delve/delve/cmd/dlv")
			resolvedPath, resolveDiagnostics := testutil.NewBufferWriter(), testutil.NewBufferWriter()
			resolve.Stdout, resolve.Stderr = resolvedPath, resolveDiagnostics
			code, resolveErr := process.RunToCompletion(ctx, executor, resolve)
			require.NoError(t, resolveErr, "%s", resolveDiagnostics.Bytes())
			require.Zero(t, code, "%s", resolveDiagnostics.Bytes())
			delve := strings.TrimSpace(string(resolvedPath.Bytes()))
			require.True(t, filepath.IsAbs(delve), "go tool must resolve an absolute Delve binary path")

			server, _, startupErr := StartTestEnvironment(t, ctx, ProjectController, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-delve-"+ending)
			project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: filepath.Dir(source),
					Process: &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
						ExecutablePath: binary, AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
					}},
					DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(),
						Args: []string{delve, "dap", "--client-addr=127.0.0.1:{{port}}"}, Mode: apiv2.DebugAdapterModeTCPCallback,
					},
				}}
			require.NoError(t, server.Client.Create(ctx, project))
			waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			connection, connectErr := connectProjectDebug(t, ctx, server.ClientConfig, project)
			require.NoError(t, connectErr)
			defer connection.Close()
			connection.SetReadLimit(9 * 1024 * 1024)
			dapClient := &projectDelveClient{connection: connection, reader: bufio.NewReader(&projectDelveStream{connection: connection})}
			var capabilities struct{ SupportsConfigurationDoneRequest bool }
			dapClient.response(t, dapClient.request(t, "initialize", map[string]any{
				"adapterID": "go", "linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path",
			}), &capabilities)
			require.True(t, capabilities.SupportsConfigurationDoneRequest)
			launch := dapClient.request(t, "launch", map[string]any{
				"mode": "exec", "program": binary, "cwd": filepath.Dir(source), "outputMode": "remote",
			})
			dapClient.event(t, "initialized", nil)
			var breakpoints struct{ Breakpoints []struct{ Verified bool } }
			dapClient.response(t, dapClient.request(t, "setBreakpoints", map[string]any{
				"source": map[string]string{"path": source}, "breakpoints": []map[string]int{{"line": breakpoint}},
			}), &breakpoints)
			require.Len(t, breakpoints.Breakpoints, 1)
			require.True(t, breakpoints.Breakpoints[0].Verified)
			dapClient.response(t, dapClient.request(t, "configurationDone", struct{}{}), nil)
			dapClient.response(t, launch, nil)
			var stopped struct {
				Reason   string
				ThreadID int `json:"threadId"`
			}
			dapClient.event(t, "stopped", &stopped)
			require.Equal(t, "breakpoint", stopped.Reason)
			require.Positive(t, stopped.ThreadID)
			var stack struct {
				StackFrames []struct {
					ID   int
					Name string
					Line int
				}
			}
			dapClient.response(t, dapClient.request(t, "stackTrace", map[string]int{"threadId": stopped.ThreadID}), &stack)
			require.NotEmpty(t, stack.StackFrames)
			require.Equal(t, "main.main", stack.StackFrames[0].Name)
			require.Equal(t, breakpoint, stack.StackFrames[0].Line)
			var scopes struct {
				Scopes []struct {
					Name               string
					VariablesReference int
				}
			}
			dapClient.response(t, dapClient.request(t, "scopes", map[string]int{"frameId": stack.StackFrames[0].ID}), &scopes)
			locals := 0
			for _, scope := range scopes.Scopes {
				if scope.Name == "Locals" {
					locals = scope.VariablesReference
				}
			}
			require.Positive(t, locals)
			var variables struct {
				Variables []struct{ Name, Value string }
			}
			dapClient.response(t, dapClient.request(t, "variables", map[string]int{"variablesReference": locals}), &variables)
			foundResult := false
			for _, variable := range variables.Variables {
				if variable.Name == "result" {
					require.Equal(t, "55", variable.Value)
					foundResult = true
				}
			}
			require.True(t, foundResult, "the computed local must be inspectable")

			observed := waitObjectAssumesStateEx(t, ctx, server.Client, project.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				return len(current.Status.Debug.Executions) == 1 && len(current.Status.Debug.Sessions[0].Processes) == 1 &&
					current.Status.Debug.Sessions[0].Processes[0].Observation.Phase == apiv2.ProjectDebugProcessRunning, nil
			})
			adapter := observed.Status.Debug.Executions[0]
			target := observed.Status.Debug.Sessions[0].Processes[0]
			require.NotNil(t, adapter.PID)
			require.NotNil(t, target.PID)
			require.False(t, target.IdentityTimestamp.IsZero())
			adapterPID, adapterPIDErr := process.Int64_ToPidT(*adapter.PID)
			require.NoError(t, adapterPIDErr)
			targetPID, targetPIDErr := process.Int64_ToPidT(*target.PID)
			require.NoError(t, targetPIDErr)
			handles := []process.ProcessHandle{
				process.NewHandle(adapterPID, adapter.IdentityTimestamp.Time),
				process.NewHandle(targetPID, target.IdentityTimestamp.Time),
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := process.WithDetachedStopTimeout(ctx)
				defer cleanupCancel()
				for _, handle := range handles {
					stopErr := executor.StopProcess(cleanupCtx, handle)
					require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
				}
			})
			if ending == "continue" {
				dapClient.response(t, dapClient.request(t, "continue", map[string]int{"threadId": stopped.ThreadID}), nil)
				var exited struct{ ExitCode int }
				dapClient.event(t, "exited", &exited)
				require.Zero(t, exited.ExitCode)
				dapClient.event(t, "terminated", nil)
				var output strings.Builder
				for _, message := range dapClient.pending {
					if string(message["event"]) == `"output"` {
						var body struct{ Output string }
						require.NoError(t, json.Unmarshal(message["body"], &body))
						output.WriteString(body.Output)
					}
				}
				require.Contains(t, output.String(), "Result: 55")
				dapClient.response(t, dapClient.request(t, "disconnect", map[string]bool{"terminateDebuggee": true}), nil)
				waitProjectPhase(t, ctx, server.Client, project, apiv2.ProjectPhaseReady)
			}
			require.NoError(t, server.Client.Delete(ctx, project))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, project)
			for _, handle := range handles {
				require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
					checkErr := executor.CheckProcessRunning(handle)
					if checkErr == nil {
						return false, nil
					}
					if process.IsProcessGoneErr(checkErr) {
						return true, nil
					}
					return false, checkErr
				}))
			}
		})
	}
}
