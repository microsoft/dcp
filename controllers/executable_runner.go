// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/process"
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
	// When the passed context is cancelled, the run should be automatically terminated.
	//
	// The runner should not try to change the Executable in any way. Instead, the runner supplies
	// new data about the run by modifying the provided startingRunInfo object.
	//
	// The runChangeHandler is used to notify the caller about the run's progress and completion,
	// see RunChangeHandler for more details.
	StartRun(
		ctx context.Context,
		exe *apiv1.Executable,
		startingRunInfo *ExecutableRunInfo,
		runChangeHandler RunChangeHandler,
		log logr.Logger,
	) error

	// Stops the run with a given ID.
	StopRun(ctx context.Context, runID RunID, log logr.Logger) error
}

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
	// The supplied runInfo object will be filled with the updated information about the run.
	//
	// The caller must call the startWaitForRunCompletion function to receive further notifications about the run.
	// For example, OnRunCompleted method call will be delayed till startWaitForRunCompletion is called.
	//
	// In case of synchronous startup, this method will be called before ExecutableRunner.StartRun() returns.
	OnStartupCompleted(name types.NamespacedName, startedRunInfo *ExecutableRunInfo, startWaitForRunCompletion func())
}
