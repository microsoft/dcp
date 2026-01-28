/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"context"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
)

func EnsureProcessTree(t *testing.T, rootP process.ProcessTreeItem, expectedSize int, timeout time.Duration) {
	processesStartedCtx, processesStartedCancelFn := context.WithTimeout(context.Background(), timeout)
	defer processesStartedCancelFn()

	var processTreeLen int
	err := wait.PollUntilContextCancel(
		processesStartedCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			processTree, err := process.GetProcessTree(rootP)
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
