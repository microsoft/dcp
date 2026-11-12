/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package commandstreams provides shared subprocess stream checks for tests.
package commandstreams

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type commandStreamExecutor struct {
	process.Executor
	tool      string
	arguments []string
	command   []string
	stdin     io.Reader
	fail      bool
}

// VerifyTarCopy checks archive-reader ownership and exact copy arguments without invoking a container runtime.
func VerifyTarCopy(t *testing.T, prefix []string, run func(context.Context, process.Executor, io.Reader) error) {
	t.Helper()
	for _, fail := range []bool{false, true} {
		name := "copy"
		if fail {
			name = "launch-failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			tool, toolErr := internal_testutil.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			owner := process.NewOSExecutor(logr.Discard())
			defer owner.Dispose()
			executor := &commandStreamExecutor{Executor: owner, tool: tool, arguments: []string{"--echo-stdio"}, fail: fail}
			archive := bytes.NewReader([]byte("tar archive payload"))
			copyErr := run(ctx, executor, archive)
			if fail {
				require.ErrorContains(t, copyErr, "injected exec start failure")
			} else {
				require.NoError(t, copyErr)
			}
			require.Same(t, archive, executor.stdin)
			expected := append(slices.Clone(prefix), "container", "cp", "-a=false", "-", "pinned-id:/workspace")
			require.Equal(t, prefix[0], strings.TrimSuffix(filepath.Base(executor.command[0]), ".exe"))
			require.Equal(t, expected[1:], executor.command[1:])
		})
	}
}

func (executor *commandStreamExecutor) StartProcess(
	ctx context.Context, cmd *exec.Cmd, handler process.ProcessExitHandler, flags process.ProcessCreationFlag, create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.command = slices.Clone(cmd.Args)
	executor.stdin = cmd.Stdin
	if executor.fail {
		return process.ProcessHandle{}, nil, errors.New("injected exec start failure")
	}
	cmd.Path, cmd.Args, cmd.Err = executor.tool, append([]string{executor.tool}, executor.arguments...), nil
	return executor.Executor.StartProcess(ctx, cmd, handler, flags, create)
}

// VerifyCommandStreams checks a command-launch boundary using the shared delay tool instead of the real CLI.
// Run must append pinned-id, adapter and --flag to prefix and use --interactive only when stdin is supplied.
func VerifyCommandStreams(t *testing.T, prefix []string,
	run func(context.Context, process.Executor, io.Reader, io.WriteCloser, io.WriteCloser) (<-chan int32, error),
) {
	t.Helper()
	waitExit := func(ctx context.Context, exits <-chan int32) int32 {
		t.Helper()
		select {
		case code, open := <-exits:
			require.True(t, open, "exit channel closed without a result")
			select {
			case _, stillOpen := <-exits:
				require.False(t, stillOpen, "exec must publish exactly one result")
			case <-ctx.Done():
				t.Fatal("attached exec did not close its result channel")
			}
			return code
		case <-ctx.Done():
			t.Fatal("attached exec did not finish")
			return 0
		}
	}
	for _, ending := range []string{"duplex", "cancel", "exit", "launch-failure", "output-only", "finite-input"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			testCtx, testCancel := testutil.GetTestContext(t, 0)
			defer testCancel()
			ctx, cancel := context.WithCancel(testCtx)
			defer cancel()
			tool, toolErr := internal_testutil.GetTestToolPath("delay")
			require.NoError(t, toolErr)
			owner := process.NewOSExecutor(logr.Discard())
			t.Cleanup(owner.Dispose)
			executor := &commandStreamExecutor{Executor: owner, tool: tool,
				arguments: []string{"--delay", "1ms"}, fail: ending == "launch-failure"}
			if ending == "duplex" || ending == "cancel" || ending == "finite-input" {
				executor.arguments = []string{"--echo-stdio"}
			}
			input, writer, pipeErr := os.Pipe()
			require.NoError(t, pipeErr)
			defer input.Close()
			defer writer.Close()
			var source io.Reader = input
			if ending == "output-only" {
				source = nil
			} else if ending == "finite-input" {
				source = bytes.NewReader([]byte{0, '\r', '\n', 0xff})
			}
			stdout, stderr := testutil.NewBufferWriter(), testutil.NewBufferWriter()
			exits, launchErr := run(ctx, executor, source, stdout, stderr)
			if source == nil {
				require.Nil(t, executor.stdin)
			} else {
				require.Same(t, source, executor.stdin)
			}
			expected := slices.Clone(prefix)
			if source != nil {
				expected = append(expected, "--interactive")
			}
			expected = append(expected, "pinned-id", "adapter", "--flag")
			require.Equal(t, prefix[0], strings.TrimSuffix(filepath.Base(executor.command[0]), ".exe"))
			require.Equal(t, expected[1:], executor.command[1:])
			if ending == "launch-failure" {
				require.ErrorContains(t, launchErr, "injected exec start failure")
				require.Nil(t, exits)
			} else {
				require.NoError(t, launchErr)
				if ending == "duplex" {
					stopInputCancellation := context.AfterFunc(testCtx, func() {
						_ = input.Close()
						_ = writer.Close()
					})
					defer stopInputCancellation()
					for _, payload := range [][]byte{
						{0, '\r', '\n', 0xff, 0x80},
						[]byte("second request\r\n"),
						bytes.Repeat([]byte{0, '\r', '\n', 0xff, 0x80}, 32768),
					} {
						before := len(stdout.Bytes())
						_, writeErr := writer.Write(payload)
						require.NoError(t, writeErr)
						require.NoError(t, wait.PollUntilContextCancel(testCtx, time.Millisecond, true, func(context.Context) (bool, error) {
							return len(stdout.Bytes()) == before+len(payload), nil
						}))
						require.Equal(t, payload, stdout.Bytes()[before:])
					}
					require.NoError(t, writer.Close())
				} else if ending == "cancel" {
					cancel()
				}
				code := waitExit(testCtx, exits)
				if ending != "cancel" {
					require.Zero(t, code)
				}
				if ending == "duplex" || ending == "finite-input" {
					require.Equal(t, "stdio echo ready\n", string(stderr.Bytes()))
				}
				if ending == "finite-input" {
					require.Equal(t, []byte{0, '\r', '\n', 0xff}, stdout.Bytes())
				}
			}
			_, statErr := input.Stat()
			require.NoError(t, statErr, "the input pipe remains caller-owned")
		})
	}
}
