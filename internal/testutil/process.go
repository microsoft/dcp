/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	stdslices "slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	testutils "github.com/microsoft/dcp/pkg/testutil"
)

type commandStreamExecutor struct {
	process.Executor
	tool      string
	arguments []string
	command   []string
	stdin     io.Reader
	fail      bool
}

func (executor *commandStreamExecutor) StartProcess(
	ctx context.Context, cmd *exec.Cmd, handler process.ProcessExitHandler, flags process.ProcessCreationFlag, create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	executor.command = stdslices.Clone(cmd.Args)
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
			testCtx, testCancel := testutils.GetTestContext(t, 0)
			defer testCancel()
			ctx, cancel := context.WithCancel(testCtx)
			defer cancel()
			tool, toolErr := GetTestToolPath("delay")
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
			stdout, stderr := testutils.NewBufferWriter(), testutils.NewBufferWriter()
			exits, launchErr := run(ctx, executor, source, stdout, stderr)
			if source == nil {
				require.Nil(t, executor.stdin)
			} else {
				require.Same(t, source, executor.stdin)
			}
			expected := stdslices.Clone(prefix)
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

func EnsureProcessTree(t *testing.T, rootP process.ProcessHandle, expectedSize int, timeout time.Duration) {
	processesStartedCtx, processesStartedCancelFn := context.WithTimeout(context.Background(), timeout)
	defer processesStartedCancelFn()

	var processTreeLen int
	err := wait.PollUntilContextCancel(
		processesStartedCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			processTree, err := process.GetProcessTree(processesStartedCtx, rootP)
			if err != nil {
				return false, err
			}
			processTreeLen = len(processTree)
			return processTreeLen == expectedSize, nil
		},
	)

	require.NoError(t, err, "expected number of 'delay' program instances not found (expected %d, actual %d)", expectedSize, processTreeLen)
}

type ProcessExecution struct {
	PID                process.Pid_t
	Cmd                *exec.Cmd
	StartWaitingCalled bool
	startWaitingChan   chan struct{}
	StartedAt          time.Time
	EndedAt            time.Time
	ExitHandler        process.ProcessExitHandler
	ExitCode           int32
	ExecutionEnded     chan struct{}
	Signal             chan syscall.Signal  // Channel to send simulated signals to the process
	Executor           *TestProcessExecutor // The reference to the executor that created this process execution.

	// Set to true when the process stop is initiated, enabling proper resource cleanup.
	stopInitiated bool
}

func (pe *ProcessExecution) Running() bool {
	return pe.EndedAt.IsZero()
}
func (pe *ProcessExecution) Finished() bool {
	return !pe.EndedAt.IsZero()
}

type ProcessSearchCriteriaCond func(pe *ProcessExecution) bool

type ProcessSearchCriteria struct {
	Command []string                  // The name/path of the executable and first N arguments, if any.
	LastArg string                    // The last argument of the command that needs to match. Optional.
	Cond    ProcessSearchCriteriaCond // Special condition to match the command. Optional.
}

func (sc *ProcessSearchCriteria) Equals(other *ProcessSearchCriteria) bool {
	if sc == nil && other == nil {
		return true
	}

	if other == nil {
		return false
	}

	if slices.SeqIndex(sc.Command, other.Command) != 0 {
		return false
	}

	if sc.LastArg != other.LastArg {
		return false
	}

	// We are not going to compare the Cond function because there is no easy way to determine
	// whether two functions "do the same thing".
	return true
}

func (sc *ProcessSearchCriteria) Matches(pe *ProcessExecution) bool {
	if !sc.MatchesCmd(pe.Cmd) {
		return false
	}

	if sc.Cond != nil && !sc.Cond(pe) {
		return false // Condition doesn't match
	}

	return true
}

func (sc *ProcessSearchCriteria) MatchesCmd(cmd *exec.Cmd) bool {
	if len(sc.Command) == 0 {
		return false
	}

	cmdPath := sc.Command[0]
	usingAbsPath := path.IsAbs(cmdPath)

	if usingAbsPath {
		if cmd.Path != cmdPath {
			return false // Path to executable does not match
		}
	} else {
		if !strings.Contains(cmd.Path, cmdPath) {
			return false // Path to executable does not contain the expected command name
		}
	}

	args := cmd.Args

	if len(args) < len(sc.Command) {
		return false // Not enough arguments
	}

	if len(sc.Command) > 1 && !slices.StartsWith(args[1:], sc.Command[1:]) {
		return false // First N arguments don't match
	}

	if sc.LastArg != "" && args[len(args)-1] != sc.LastArg {
		return false // Last argument doesn't match
	}

	return true
}

// AutoExecution structure is used by clients to automatically and asynchronously complete
// an execution of a command that matches certain criteria.
type AutoExecution struct {
	// The criteria that needs to be matched for the command to be executed.
	// There is no safeguard against multiple commands matching the criteria.
	Condition ProcessSearchCriteria

	// The RunCommand function is called after the process is "running".
	// It can write to command stdout and stderr. The return value is an exit code for the command.
	// The function should be checking pe.Signal channel to see if any signals were sent to the process.
	// The test convention is that execution should stop when SIGTERM is received.
	RunCommand func(pe *ProcessExecution) int32

	// If not nil, this is the error that will be returned by the Executor from StartProcess() call.
	// RunCommand will not be called in this case.
	StartupError func(*ProcessExecution) error

	// If not nil, the process will fail to stop with the specified error.
	StopError func(*ProcessExecution) error

	// AsynchronousStartupDelay is used to simulate asynchronous process startup.
	// If set to a non-zero value, the process startup will be delayed by the specified duration.
	AsynchronousStartupDelay time.Duration
}
