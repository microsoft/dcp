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
	// Runs the Executable. When the passed context is cancelled, the run is automatically terminated.
	// Returns the run ID, and a function that enables run completion notifications delivered to the exit handler.
	StartRun(
		ctx context.Context,
		exe *apiv1.Executable,
		runInfo *ExecutableRunInfo,
		runChangeHandler RunChangeHandler,
		log logr.Logger,
	) error

	// Stops the run with a given ID.
	StopRun(ctx context.Context, runID RunID, log logr.Logger) error
}

type RunChangeHandler interface {
	// Called when the Executable run changes.
	// If err is nil, the PID and (optionally) process exit code were properly captured and the exitCode value (if supplied) is valid.
	// if err is not nil, there was a problem with the run and the PID and exitCode value are not valid
	// (and should be UnknwonPID and UnknownExitCode respectively).
	OnRunChanged(runID RunID, pid process.Pid_t, exitCode *int32, err error)

	// Called when the run has completed.
	// If err is nil, the run completed successfully and the exitCode value (if supplied) is valid.
	// If err is not nil, the run did not complete successfully and the exitCode value is not valid.
	// (and should be UnknownExitCode).
	OnRunCompleted(runID RunID, exitCode *int32, err error)

	// Called when a run has started and wants to register its RunID with the handler.
	OnStartingCompleted(name types.NamespacedName, runID RunID, runInfo *ExecutableRunInfo, startWaitForRunCompletion func())
}
