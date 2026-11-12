/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dap"
	internaltest "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that network adapter buffering fails independently of downstream reads on overflow,
// joins its reader on close, and preserves ordinary buffered output through EOF.
func TestProjectDebugNetworkOutputBuffer(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprint(overflow), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			source, upstream := net.Pipe()
			transport := bufferProjectDebugTransport(&projectDebugTransport{
				Reader: source, Writer: source, close: source.Close,
			})
			defer transport.Close()
			defer upstream.Close()
			stopCancellation := context.AfterFunc(ctx, func() { _ = transport.Close() })
			defer stopCancellation()
			written := make(chan error, 1)
			go func() {
				if overflow {
					chunk := bytes.Repeat([]byte("x"), 64*1024)
					for total := 0; total <= projectDebugOutputLimit; total += len(chunk) {
						if _, writeErr := upstream.Write(chunk); writeErr != nil {
							written <- writeErr
							return
						}
					}
					written <- nil
				} else {
					_, writeErr := upstream.Write([]byte("final output"))
					_ = upstream.Close()
					written <- writeErr
				}
			}()
			if overflow {
				select {
				case <-transport.failure.Done():
					require.ErrorIs(t, context.Cause(transport.failure), usvc_io.ErrBufferFull)
				case <-ctx.Done():
					t.Fatal("network buffer overflow did not signal session failure")
				}
				require.NoError(t, transport.Close())
				<-written
			} else {
				payload, readErr := io.ReadAll(transport)
				require.NoError(t, readErr)
				require.Equal(t, "final output", string(payload))
				require.NoError(t, <-written)
				require.NoError(t, transport.Close())
				require.NoError(t, transport.failure.Err())
			}
		})
	}
}

type blockedDebugForwardingWriter struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (writer *blockedDebugForwardingWriter) Write([]byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	<-writer.closed
	return 0, io.ErrClosedPipe
}

// Verifies that the common stdio/TCP buffer bounds blocked final-output forwarding on upstream
// EOF even while the adapter execution remains live.
func TestProjectDebugConnectionEOFFinishesBlockedSession(t *testing.T) {
	t.Parallel()
	for _, stdio := range []bool{false, true} {
		t.Run(fmt.Sprintf("stdio=%t", stdio), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			var source io.ReadCloser
			var upstream io.WriteCloser
			if stdio {
				source, upstream = io.Pipe()
			} else {
				source, upstream = net.Pipe()
			}
			transport := bufferProjectDebugTransport(&projectDebugTransport{
				Reader: source, Writer: io.Discard, close: source.Close,
			})
			defer transport.Close()
			defer upstream.Close()
			executionDone := make(chan struct{})
			input, inputWriter := io.Pipe()
			defer inputWriter.Close()
			blocked := &blockedDebugForwardingWriter{entered: make(chan struct{}), closed: make(chan struct{})}
			client := &projectDebugTransport{Reader: input, Writer: blocked, close: func() error {
				close(blocked.closed)
				return input.Close()
			}}
			defer client.Close()
			completed := make(chan error, 1)
			go func() {
				completed <- dap.Bridge(ctx,
					&debugDAPTransport{reader: bufio.NewReader(client), writer: client, close: client.Close},
					&debugDAPTransport{reader: bufio.NewReader(transport), writer: transport, close: transport.Close},
					executionDone, transport.readDone, apiv2.ProjectDebugShutdownGrace, nil)
			}()
			require.NoError(t, dap.WriteMessage(upstream, dap.Message{
				"seq": json.RawMessage("1"), "type": json.RawMessage(`"event"`),
				"event": json.RawMessage(`"terminated"`),
			}))
			select {
			case <-blocked.entered:
			case <-ctx.Done():
				t.Fatal("final output did not reach blocked client")
			}
			require.NoError(t, upstream.Close())
			select {
			case bridgeErr := <-completed:
				require.ErrorContains(t, bridgeErr, "output drain timed out")
			case <-ctx.Done():
				t.Fatal("connection EOF left final output blocked")
			}
			select {
			case <-executionDone:
				t.Fatal("session completion ended its shared execution")
			default:
			}
		})
	}
}

// Verifies that host and container-exec output allows independent completion under backpressure,
// and that overflow fails a blocked session and permits execution cleanup without client reads.
// The childless echo fixture may exit during tree enumeration; its captured identity must still be gone.
func TestProjectDebugCompletionDoesNotWaitForForwarding(t *testing.T) {
	for _, placement := range []string{"host", "container"} {
		for _, overflow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overflow=%t", placement, overflow), func(t *testing.T) {
				t.Setenv("DCP_DISABLE_MONITOR_PROCESS", "true")
				ctx, cancel := testutil.GetTestContext(t, 0)
				t.Cleanup(cancel)
				apiClient, statusWriter := startProjectDebugStatusServer(t, ctx)
				executor := process.NewOSExecutor(logr.Discard())
				t.Cleanup(executor.Dispose)
				tool, toolErr := internaltest.GetTestToolPath("delay")
				require.NoError(t, toolErr)
				project := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "blocked-forwarding"},
					Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(),
						Process:      &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "unused"}},
						DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{tool, "--dap-stdio"}}}}
				if overflow {
					project.Spec.DebugAdapter.Args[1] = "--echo-stdio"
				}
				if placement == "container" {
					project.Spec.Process = nil
					project.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "unused"}}
				}
				require.NoError(t, apiClient.Create(ctx, project))
				debugRuntime := newProjectDebugRuntime(ctx, executor, statusWriter, logr.Discard())
				var adapterHandle process.ProcessHandle
				if placement == "container" {
					project.Status.ContainerID, project.Status.ContainerRuntime = "native-cli-target", "native-cli-runtime"
					require.NoError(t, apiClient.Status().Update(ctx, project))
					containerExec := &containerAdapterExecutor{run: func(execCtx context.Context, options containers.ExecContainerOptions) (<-chan int32, error) {
						require.IsType(t, (*io.PipeWriter)(nil), options.StdOutStream)
						cmd := exec.Command(options.Command, options.Args...)
						cmd.Stdin, cmd.Stdout, cmd.Stderr = options.StdInStream, options.StdOutStream, options.StdErrStream
						exits := make(chan int32, 1)
						handle, startWait, startErr := executor.StartProcess(execCtx, cmd, process.ProcessExitHandlerFunc(func(_ process.Pid_t, code int32, _ error) {
							exits <- code
							close(exits)
						}), process.CreationFlagEnsureKillOnDispose, nil)
						if startErr != nil {
							return nil, startErr
						}
						adapterHandle = handle
						startWait()
						return exits, nil
					}}
					debugRuntime.findContainerRuntime = func(context.Context, string) (containers.ExecContainers, error) {
						return containerExec, nil
					}
				}
				execution, adapterStream, launchErr := debugRuntime.launch(ctx, project, apiv2.DebugAdapterModeStdio)
				require.NoError(t, launchErr)
				adapterStream = bufferProjectDebugTransport(adapterStream)
				if placement == "host" {
					adapterHandle = execution.handle
				}
				t.Cleanup(func() {
					require.NoError(t, adapterStream.Close())
					stopErr := execution.stop()
					if overflow && placement == "container" {
						require.ErrorContains(t, stopErr, "in-container adapter termination is unconfirmed")
					} else if overflow && errors.Is(stopErr, process.ErrIncompleteProcessTree) {
						require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(adapterHandle)))
					} else {
						require.NoError(t, stopErr)
					}
				})
				input, inputWriter := io.Pipe()
				defer inputWriter.Close()
				blocked := &blockedDebugForwardingWriter{entered: make(chan struct{}), closed: make(chan struct{})}
				clientStream := &projectDebugTransport{Reader: input, Writer: blocked, close: func() error {
					close(blocked.closed)
					return input.Close()
				}}
				client := &debugDAPTransport{reader: bufio.NewReader(input), writer: blocked, close: clientStream.Close}
				adapter := &debugDAPTransport{reader: bufio.NewReader(adapterStream.Reader), writer: adapterStream.Writer, close: adapterStream.Close}
				completed := make(chan error, 1)
				bridgeCtx, cancelBridge := context.WithCancelCause(ctx)
				defer cancelBridge(nil)
				stopOutputFailure := context.AfterFunc(adapterStream.failure, func() {
					cancelBridge(context.Cause(adapterStream.failure))
				})
				defer stopOutputFailure()
				go func() {
					defer close(completed)
					completed <- dap.Bridge(bridgeCtx, client, adapter, execution.done, adapterStream.readDone, apiv2.ProjectDebugShutdownGrace, nil)
				}()
				t.Cleanup(func() {
					_ = clientStream.Close()
					_ = adapterStream.Close()
					cancel()
					<-completed
				})
				require.NoError(t, dap.WriteMessage(inputWriter, dap.Message{
					"seq": json.RawMessage("1"), "type": json.RawMessage(`"request"`), "command": json.RawMessage(`"reportProcess"`),
					"arguments": json.RawMessage(`{"isLocalProcess":false}`),
				}))
				select {
				case <-blocked.entered:
				case <-ctx.Done():
					t.Fatal("adapter output did not reach the blocked forwarder")
				}
				if overflow {
					floodDone := make(chan struct{})
					payload, encodeErr := json.Marshal(string(bytes.Repeat([]byte("x"), 64*1024)))
					require.NoError(t, encodeErr)
					go func() {
						defer close(floodDone)
						for sequence := int64(2); ; sequence++ {
							message := dap.Message{
								"type": json.RawMessage(`"event"`), "event": json.RawMessage(`"output"`),
								"body": payload,
							}
							message["seq"] = json.RawMessage(fmt.Sprint(sequence))
							if floodErr := dap.WriteMessage(inputWriter, message); floodErr != nil {
								return
							}
						}
					}()
					select {
					case bridgeErr := <-completed:
						require.ErrorIs(t, bridgeErr, usvc_io.ErrBufferFull)
					case <-ctx.Done():
						t.Fatal("overflow did not release blocked forwarding")
					}
					require.NoError(t, inputWriter.Close())
					<-floodDone
					require.NoError(t, adapterStream.Close())
					stopErr := execution.stop()
					if placement == "container" {
						require.ErrorContains(t, stopErr, "in-container adapter termination is unconfirmed")
					} else if errors.Is(stopErr, process.ErrIncompleteProcessTree) {
						require.True(t, process.IsProcessGoneErr(executor.CheckProcessRunning(adapterHandle)))
					} else {
						require.NoError(t, stopErr)
					}
					require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
						return process.IsProcessGoneErr(executor.CheckProcessRunning(adapterHandle)), nil
					}))
					return
				}
				require.NoError(t, dap.WriteMessage(inputWriter, dap.Message{
					"seq": json.RawMessage("2"), "type": json.RawMessage(`"request"`), "command": json.RawMessage(`"disconnect"`),
					"arguments": json.RawMessage(`{}`),
				}))
				require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
					return process.IsProcessGoneErr(executor.CheckProcessRunning(adapterHandle)), nil
				}))
				drainCtx, stopDrain := context.WithTimeout(ctx, 3*time.Second)
				defer stopDrain()
				select {
				case bridgeErr := <-completed:
					require.ErrorContains(t, bridgeErr, "output drain timed out")
				case <-drainCtx.Done():
					t.Fatal("process exit did not release blocked forwarding within the drain bound")
				}
				require.NoError(t, apiClient.Get(ctx, project.NamespacedName(), project))
				require.Equal(t, apiv2.ProjectDebugPhaseExited, project.Status.Debug.Executions[0].Phase)
				require.NotNil(t, project.Status.Debug.Executions[0].ExitCode)
				require.Zero(t, *project.Status.Debug.Executions[0].ExitCode)
			})
		}
	}
}
