// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

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
	if len(sc.Command) == 0 {
		return false
	}

	cmdPath := sc.Command[0]
	usingAbsPath := path.IsAbs(cmdPath)

	if usingAbsPath {
		if pe.Cmd.Path != cmdPath {
			return false // Path to executable does not match
		}
	} else {
		if !strings.Contains(pe.Cmd.Path, cmdPath) {
			return false // Path to executable does not contain the expected command name
		}
	}

	args := pe.Cmd.Args

	if len(args) < len(sc.Command) {
		return false // Not enough arguments
	}

	if len(sc.Command) > 1 && !slices.StartsWith(args[1:], sc.Command[1:]) {
		return false // First N arguments don't match
	}

	if sc.LastArg != "" && args[len(args)-1] != sc.LastArg {
		return false // Last argument doesn't match
	}

	if sc.Cond != nil && !sc.Cond(pe) {
		return false // Condition doesn't match
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
}

type TestProcessExecutor struct {
	nextPID        int64
	Executions     []*ProcessExecution
	AutoExecutions []AutoExecution
	m              *sync.RWMutex
	lifetimeCtx    context.Context
}

const (
	NotFound              = -1
	KilledProcessExitCode = 137 // 128 + SIGKILL (9)
)

func NewTestProcessExecutor(lifetimeCtx context.Context) *TestProcessExecutor {
	return &TestProcessExecutor{
		m:           &sync.RWMutex{},
		lifetimeCtx: lifetimeCtx,
	}
}

func (e *TestProcessExecutor) Close() error {
	e.m.Lock()
	defer e.m.Unlock()

	var closeErrors error

	closeStoringError := func(w io.Writer) {
		if closer, ok := w.(io.Closer); ok {
			err := closer.Close()
			if err != nil && !errors.Is(err, os.ErrClosed) {
				closeErrors = errors.Join(closeErrors, err)
			}
		}
	}

	for _, pe := range e.Executions {
		closeStoringError(pe.Cmd.Stdout)
		closeStoringError(pe.Cmd.Stderr)
	}

	return closeErrors
}

func (e *TestProcessExecutor) Dispose() {
	e.Close()
}

func (e *TestProcessExecutor) StartProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	handler process.ProcessExitHandler,
	_ process.ProcessCreationFlag,
) (process.Pid_t, time.Time, func(), error) {
	pid64 := atomic.AddInt64(&e.nextPID, 1)
	pid, err := process.Int64_ToPidT(pid64)
	if err != nil {
		return process.UnknownPID, time.Time{}, nil, err
	}

	e.m.Lock()
	defer e.m.Unlock()

	startTimestamp := time.Now()
	pe := ProcessExecution{
		Cmd:                cmd,
		PID:                pid,
		StartedAt:          startTimestamp,
		ExitHandler:        handler,
		StartWaitingCalled: false,
		Executor:           e,
	}

	// For testing purposes make sure that stdout and stderr are always captured.
	if cmd.Stdout == nil {
		cmd.Stdout = usvc_io.NopWriteCloser(new(bytes.Buffer))
	}
	if cmd.Stderr == nil {
		cmd.Stderr = usvc_io.NopWriteCloser(new(bytes.Buffer))
	}
	if handler != nil {
		pe.startWaitingChan = make(chan struct{})
	}

	e.Executions = append(e.Executions, &pe)

	startWaitingForExit := func() {
		e.m.Lock()
		defer e.m.Unlock()
		i := e.findByPid(pid)
		if i == NotFound {
			return // This can happen if a test calls ClearHistory() before calling this function.
		}
		updatedPE := e.Executions[i]
		if !updatedPE.StartWaitingCalled {
			updatedPE.StartWaitingCalled = true
			if updatedPE.startWaitingChan != nil {
				close(updatedPE.startWaitingChan)
			}
		}
		e.Executions[i] = updatedPE
	}

	if autoExecutionErr := e.maybeAutoExecute(&pe); autoExecutionErr != nil {
		return process.UnknownPID, time.Time{}, nil, autoExecutionErr
	}

	return pid, startTimestamp, startWaitingForExit, nil
}

func (e *TestProcessExecutor) StartAndForget(cmd *exec.Cmd, _ process.ProcessCreationFlag) (process.Pid_t, time.Time, error) {
	pid64 := atomic.AddInt64(&e.nextPID, 1)
	pid, err := process.Int64_ToPidT(pid64)
	if err != nil {
		return process.UnknownPID, time.Time{}, err
	}

	e.m.Lock()
	defer e.m.Unlock()

	startTimestamp := time.Now()
	pe := ProcessExecution{
		Cmd:                cmd,
		PID:                pid,
		StartedAt:          startTimestamp,
		StartWaitingCalled: false,
		Executor:           e,
	}

	// For testing purposes make sure that stdout and stderr are always captured.
	if cmd.Stdout == nil {
		cmd.Stdout = usvc_io.NopWriteCloser(new(bytes.Buffer))
	}
	if cmd.Stderr == nil {
		cmd.Stderr = usvc_io.NopWriteCloser(new(bytes.Buffer))
	}

	e.Executions = append(e.Executions, &pe)

	if autoExecutionErr := e.maybeAutoExecute(&pe); autoExecutionErr != nil {
		return process.UnknownPID, time.Time{}, autoExecutionErr
	}

	return pid, startTimestamp, nil
}

func (e *TestProcessExecutor) maybeAutoExecute(pe *ProcessExecution) error {
	if len(e.AutoExecutions) > 0 {
		for _, ae := range e.AutoExecutions {
			if ae.Condition.Matches(pe) {
				if ae.StartupError != nil {
					return ae.StartupError(pe)
				} else {
					eeChan := make(chan struct{})
					pe.ExecutionEnded = eeChan
					pe.Signal = make(chan syscall.Signal, 1)

					go func() {
						exitCode := ae.RunCommand(pe)
						close(eeChan)
						e.m.Lock()
						stopInitiated := pe.stopInitiated
						e.m.Unlock()
						if !stopInitiated {
							// RunCommand() "ended on its own" (as opposed to being triggered by StopProcess() or SimulateProcessExit()),
							// so we need to do the resource cleanup.
							stopProcessErr := e.stopProcessImpl(pe.PID, pe.StartedAt, exitCode)
							if stopProcessErr != nil && ae.StopError == nil {
								panic(fmt.Errorf("we should have an execution with PID=%d: %w", pe.PID, stopProcessErr))
							}
						}
					}()
					break
				}
			}
		}
	}

	return nil
}

// Called by the controller (via Executor interface)
func (e *TestProcessExecutor) StopProcess(pid process.Pid_t, processStartTime time.Time) error {
	return e.stopProcessImpl(pid, processStartTime, KilledProcessExitCode)
}

// Called by tests to simulate a process exit with specific exit code.
func (e *TestProcessExecutor) SimulateProcessExit(t *testing.T, pid process.Pid_t, exitCode int32) {
	err := e.stopProcessImpl(pid, time.Time{}, exitCode)
	if err != nil {
		require.Failf(t, "invalid PID (test issue)", err.Error())
	}
}

// Finds all executions of a specific command.
// The command is identified by path to the executable and a subset of its arguments (the first N arguments).
// If lastArg parameter is not empty, it is matched against the last argument of the command.
// Last parameter is a function that is called to verify that the command is the one we are waiting for.
func (e *TestProcessExecutor) FindAll(
	command []string,
	lastArg string,
	cond ProcessSearchCriteriaCond,
) []*ProcessExecution {
	e.m.RLock()
	defer e.m.RUnlock()

	retval := make([]*ProcessExecution, 0)
	if len(command) == 0 {
		return retval
	}

	sc := ProcessSearchCriteria{
		Command: command,
		LastArg: lastArg,
		Cond:    cond,
	}

	for _, pe := range e.Executions {
		if sc.Matches(pe) {
			retval = append(retval, pe)
		}
	}

	return retval
}

func (e *TestProcessExecutor) FindByPid(pid process.Pid_t) (*ProcessExecution, bool) {
	e.m.RLock()
	defer e.m.RUnlock()

	i := e.findByPid(pid)
	if i == NotFound {
		return nil, false
	}
	return e.Executions[i], true
}

// Clears all execution history
func (e *TestProcessExecutor) ClearHistory() {
	e.m.Lock()
	defer e.m.Unlock()

	e.Executions = nil
	// The PID counter is not reset so that the clients continue to receive unique PIDs.
}

func (e *TestProcessExecutor) InstallAutoExecution(autoExecution AutoExecution) {
	e.m.Lock()
	defer e.m.Unlock()

	// Remove any previous AutoExecution that matches the same criteria.
	withoutExisting := slices.Select(e.AutoExecutions, func(existing AutoExecution) bool {
		return !autoExecution.Condition.Equals(&existing.Condition)
	})

	e.AutoExecutions = append(withoutExisting, autoExecution)
}

func (e *TestProcessExecutor) RemoveAutoExecution(sc ProcessSearchCriteria) {
	e.m.Lock()
	defer e.m.Unlock()

	e.AutoExecutions = slices.Select(e.AutoExecutions, func(ae AutoExecution) bool {
		return !sc.Equals(&ae.Condition)
	})
}

func (e *TestProcessExecutor) findByPid(pid process.Pid_t) int {
	for i, pe := range e.Executions {
		if pe.PID == pid {
			return i
		}
	}

	return NotFound
}

func (e *TestProcessExecutor) stopProcessImpl(pid process.Pid_t, processStartTime time.Time, exitCode int32) error {
	e.m.Lock()

	i := e.findByPid(pid)
	if i == NotFound {
		e.m.Unlock()
		return fmt.Errorf("no process with PID %d found", pid)
	}

	if !processStartTime.IsZero() {
		if !osutil.Within(processStartTime, e.Executions[i].StartedAt, process.ProcessIdentityTimeMaximumDifference) {
			e.m.Unlock()
			return fmt.Errorf("process start time mismatch for PID %d: expected %s, actual %s",
				pid,
				processStartTime.Format(osutil.RFC3339MiliTimestampFormat),
				e.Executions[i].StartedAt.Format(osutil.RFC3339MiliTimestampFormat),
			)
		}
	}

	pe := e.Executions[i]
	pe.stopInitiated = true

	if len(e.AutoExecutions) > 0 {
		for _, ae := range e.AutoExecutions {
			if !ae.Condition.Matches(pe) {
				continue
			}

			if ae.StopError != nil {
				e.m.Unlock()
				return ae.StopError(pe)
			}

			if pe.Signal != nil {
				pe.Signal <- syscall.SIGTERM
			}
		}
	}

	if pe.ExecutionEnded != nil {
		e.m.Unlock()
		<-pe.ExecutionEnded
		e.m.Lock()
	}

	pe.ExitCode = exitCode
	pe.EndedAt = time.Now()
	e.Executions[i] = pe

	if closer, ok := pe.Cmd.Stdout.(io.Closer); ok {
		_ = closer.Close()
	}
	if closer, ok := pe.Cmd.Stderr.(io.Closer); ok {
		_ = closer.Close()
	}

	if pe.ExitHandler != nil {
		go func() {
			select {
			case <-e.lifetimeCtx.Done():
				return
			case <-pe.startWaitingChan:
				pe.ExitHandler.OnProcessExited(pid, exitCode, nil)
			}
		}()
	}

	e.m.Unlock()
	return nil
}

var _ process.Executor = (*TestProcessExecutor)(nil)
