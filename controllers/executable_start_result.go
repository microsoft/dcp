// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/pointers"
)

type ExecutableStartResult struct {
	// State of the run after the start attempt.
	// One of: Running, FailedToStart, or Finished
	ExeState apiv1.ExecutableState

	// Process ID of the process that runs the Executable
	Pid *int64

	// Run ID for the Executable
	RunID RunID

	// Paths to captured standard output and standard error files
	StdOutFile string
	StdErrFile string

	// Timestamp for when the startup attempt was completed
	CompletionTimestamp metav1.MicroTime

	// The error that occurred during startup, if any.
	// NOTE: if StartupError is nil, this does NOT necessarily mean that the startup was successful.
	// The ExeState field must be checked to determine the actual outcome of the startup attempt.
	// StartupError is an additional, explanatory error information.
	StartupError error

	// The function to call to indicate that the run change handler is ready
	// to receive further updates about the run.
	StartWaitForRunCompletion func()
}

func NewExecutableStartResult() *ExecutableStartResult {
	return &ExecutableStartResult{
		ExeState: apiv1.ExecutableStateStarting,
		Pid:      apiv1.UnknownPID,
		RunID:    UnknownRunID,
	}
}

func (res *ExecutableStartResult) Equal(other *ExecutableStartResult) bool {
	if res == nil && other == nil {
		return true
	}
	if res == nil || other == nil {
		return false
	}

	if res.ExeState != other.ExeState {
		return false
	}

	if !pointers.EqualValue(res.Pid, other.Pid) {
		return false
	}

	if res.RunID != other.RunID {
		return false
	}

	if res.StdOutFile != other.StdOutFile {
		return false
	}

	if res.StdErrFile != other.StdErrFile {
		return false
	}

	if !osutil.MicroEqual(res.CompletionTimestamp, other.CompletionTimestamp) {
		return false
	}

	// Check for error presence, not if errors are "equal" (which is not well defined).
	// We do not have a scenario where we set the StartupError more than once on a given result,
	// so this is sufficient.
	if (res.StartupError == nil) != (other.StartupError == nil) {
		return false
	}

	// StartWaitForRunCompletion function is not compared.
	// In Go function are not comparable (other than comparing to nil).

	return true
}

// Updates the given ExecutableRunInfo with the data from the ExecutableStartResult.
func (res *ExecutableStartResult) applyTo(ri *ExecutableRunInfo) {
	if res == nil || ri == nil {
		return
	}
	ri.ExeState = res.ExeState
	ri.Pid = res.Pid
	ri.RunID = res.RunID
	ri.StdOutFile = res.StdOutFile
	ri.StdErrFile = res.StdErrFile
	ri.StartupTimestamp = res.CompletionTimestamp
	if res.ExeState == apiv1.ExecutableStateFinished {
		ri.FinishTimestamp = res.CompletionTimestamp
	}
}

func (res *ExecutableStartResult) IsSuccessfullyCompleted() bool {
	if res == nil {
		return false
	}
	return res.ExeState == apiv1.ExecutableStateRunning || res.ExeState == apiv1.ExecutableStateFinished
}

func (res *ExecutableStartResult) Clone() *ExecutableStartResult {
	if res == nil {
		return nil
	}
	return &ExecutableStartResult{
		ExeState:                  res.ExeState,
		Pid:                       pointers.Duplicate(res.Pid),
		RunID:                     res.RunID,
		StdOutFile:                res.StdOutFile,
		StdErrFile:                res.StdErrFile,
		CompletionTimestamp:       res.CompletionTimestamp,
		StartupError:              res.StartupError,
		StartWaitForRunCompletion: res.StartWaitForRunCompletion,
	}
}

func (res *ExecutableStartResult) UpdateFrom(other *ExecutableStartResult) bool {
	if other == nil || res == nil {
		return false
	}

	updated := false

	if other.ExeState != apiv1.ExecutableStateEmpty && other.ExeState != apiv1.ExecutableStateStarting && res.ExeState != other.ExeState {
		res.ExeState = other.ExeState
		updated = true
	}

	if other.Pid != apiv1.UnknownPID && !pointers.EqualValue(res.Pid, other.Pid) {
		pointers.SetValueFrom(&res.Pid, other.Pid)
		updated = true
	} else if other.ExeState.IsTerminal() && res.Pid != apiv1.UnknownPID {
		// Clear PID when the run has finished.
		res.Pid = apiv1.UnknownPID
		updated = true
	}

	if other.RunID != UnknownRunID && res.RunID != other.RunID {
		res.RunID = other.RunID
		updated = true
	}

	if other.StdOutFile != "" && res.StdOutFile != other.StdOutFile {
		res.StdOutFile = other.StdOutFile
		updated = true
	}

	if other.StdErrFile != "" && res.StdErrFile != other.StdErrFile {
		res.StdErrFile = other.StdErrFile
		updated = true
	}

	updated = setTimestampIfAfterOrUnknown(other.CompletionTimestamp, &res.CompletionTimestamp) || updated

	if other.StartupError != nil && res.StartupError != other.StartupError {
		res.StartupError = other.StartupError
		updated = true
	}

	if other.StartWaitForRunCompletion != nil {
		updated = (res.StartWaitForRunCompletion == nil) || updated
		res.StartWaitForRunCompletion = other.StartWaitForRunCompletion
	}

	return updated
}

func (res *ExecutableStartResult) String() string {
	if res == nil {
		return "{}"
	}

	return fmt.Sprintf("{exeState: %s, pid: %v, runID: %s, stdOutFile: %s, stdErrFile: %s, startupCompletedTimestamp: %s, startupError: %s}",
		res.ExeState,
		logger.IntPtrValToString(res.Pid),
		logger.FriendlyString(string(res.RunID)),
		logger.FriendlyString(res.StdOutFile),
		logger.FriendlyString(res.StdErrFile),
		logger.FriendlyMetav1Timestamp(res.CompletionTimestamp),
		logger.FriendlyErrorString(res.StartupError),
	)
}
