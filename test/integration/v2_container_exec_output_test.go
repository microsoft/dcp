/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/docker"
	"github.com/microsoft/dcp/internal/podman"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/wslc"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type containerExecOutputExecutor struct {
	process.Executor
	tool      string
	drain     bool
	handle    process.ProcessHandle
	waitDelay time.Duration
}

func (executor *containerExecOutputExecutor) StartProcess(
	ctx context.Context, cmd *exec.Cmd, handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag, sysCreateProcess process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.waitDelay = cmd.WaitDelay
	cmd.Path = executor.tool
	cmd.Args = []string{executor.tool, "--child-spec", "1", "--hold-pipe"}
	cmd.Err = nil
	if executor.drain {
		cmd.Args = append(cmd.Args, "--delay", "1s")
	}
	handle, startWait, startErr := executor.Executor.StartProcess(ctx, cmd, handler, flags, sysCreateProcess)
	executor.handle = handle
	return handle, startWait, startErr
}

// Verifies that runtime exec adapters preserve output drained before completion,
// but complete within a bounded wait when a surviving descendant holds both output pipes open.
func TestV2ContainerExecAdaptersBoundOutputDrain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix root-only termination to leave a descendant holding output pipes")
	}
	t.Parallel()
	adapters := []struct {
		name string
		new  func(logr.Logger, process.Executor) containers.ContainerOrchestrator
	}{
		{name: "docker", new: docker.NewDockerCliOrchestrator},
		{name: "podman", new: podman.NewPodmanCliOrchestrator},
		{name: "wslc", new: wslc.NewWslcCliOrchestrator},
	}
	for _, adapter := range adapters {
		for _, holdOutput := range []bool{false, true} {
			name := adapter.name + "/drained"
			if holdOutput {
				name = adapter.name + "/held-open"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				osExecutor := process.NewOSExecutor(logr.Discard())
				defer osExecutor.Dispose()
				tool, toolErr := internal_testutil.GetTestToolPath("delay")
				require.NoError(t, toolErr)
				executor := &containerExecOutputExecutor{Executor: osExecutor, tool: tool, drain: !holdOutput}
				orchestrator := adapter.new(logr.Discard(), executor)
				var stdout, stderr bytes.Buffer
				completion, launchErr := orchestrator.ExecContainer(ctx, containers.ExecContainerOptions{
					Container: "test-container", Command: "test-command",
					StreamCommandOptions: containers.StreamCommandOptions{
						StdOutStream: usvc_io.NopWriteCloser(&stdout), StdErrStream: usvc_io.NopWriteCloser(&stderr),
					},
				})
				require.NoError(t, launchErr)
				require.Equal(t, 2*time.Second, executor.waitDelay)
				var tree []process.ProcessHandle
				require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(treeCtx context.Context) (bool, error) {
					var treeErr error
					tree, treeErr = process.GetProcessTree(treeCtx, executor.handle)
					return len(tree) > 1, treeErr
				}))
				defer func() {
					for _, child := range tree {
						stopErr := osExecutor.StopProcess(context.WithoutCancel(ctx), child)
						require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
					}
				}()
				root, rootErr := executor.handle.OsProcess()
				require.NoError(t, rootErr)
				require.NoError(t, root.Kill())
				require.NoError(t, root.Release())
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				select {
				case _, valid := <-completion:
					require.True(t, valid, "completion must report an exit result")
				case <-timer.C:
					t.Fatal("exec completion exceeded the bounded output-drain allowance")
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if holdOutput {
					require.NoError(t, osExecutor.CheckProcessRunning(tree[1]),
						"completion must not require termination of a descendant holding output open")
				} else {
					require.Contains(t, stdout.String(), "Ran to completion\n")
				}
			})
		}
	}
}
