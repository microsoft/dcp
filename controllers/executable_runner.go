/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"

	"github.com/microsoft/dcp/pkg/process"
)

type RunID string

const (
	UnknownRunID RunID = ""
)

// ExecutableRunner is an entity that knows how to "run" an executable.
// Examples include ordinary (OS) process runner and IDE runner (which runs the executable inside IDE like VS or VS Code).
type ExecutableRunner interface {
	// Runs the Executable.
	//
	// The runChangeHandler is used to notify the caller about the run's progress and completion, see RunChangeHandler for more details.
	//
	// The following contract should be observed by the ExecutableRunner implementation:
	// -- When the passed context is cancelled, the run should be automatically terminated.
	// -- The runner should not try to change the passed Executable in any way.
	// -- The method should always return a result (no nil return value).
	StartRun(
		ctx context.Context,
		exe *apiv1.Executable,
		runChangeHandler RunChangeHandler,
		log logr.Logger,
	) *ExecutableStartResult

	// Stops the run with a given ID.
	StopRun(ctx context.Context, runID RunID, log logr.Logger) error
}

type RunMessageLevel string

const (
	RunMessageLevelInfo  RunMessageLevel = "info"
	RunMessageLevelDebug RunMessageLevel = "debug"
	RunMessageLevelError RunMessageLevel = "error"
)

type RunChangeHandler interface {
	// Called when the main process of the run changes (is started or re-started).
	//
	// Note: if the startup is synchronous, this method may never be called. Instead, the process ID
	// for the main process of the run will be reported via the runInfo parameter of the OnStartupCompleted() notification.
	OnMainProcessChanged(runID RunID, pid process.Pid_t)

	// Called when the run has completed.
	// If err is nil, the run completed successfully and the exitCode value (if supplied) is valid.
	// If err is not nil, the run did not complete successfully and the exitCode value is not valid.
	// (and should be UnknownExitCode).
	OnRunCompleted(runID RunID, exitCode *int32, err error)

	// Called when startup has been completed for a run.
	// The name parameter contains the name of the Executable that was started (or attempted to start).
	// The result parameter contains the outcome of the startup attempt.
	//
	// The caller must call the result.StartWaitForRunCompletion function to receive further notifications about the run.
	// For example, OnRunCompleted method call will be delayed till StartWaitForRunCompletion is called.
	//
	// In case of synchronous startup, this method will be called before ExecutableRunner.StartRun() returns.
	OnStartupCompleted(name types.NamespacedName, result *ExecutableStartResult)

	// Called when the runner needs to emit a user-targeted diagnostic message about a run.
	OnRunMessage(runID RunID, level RunMessageLevel, message string)
}
