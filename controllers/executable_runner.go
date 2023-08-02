// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

type RunID string

// ExecutableRunner is an entity that knows how to "run" an executable.
// Examples include ordinary (OS) process runner and IDE runner (which runs the executable inside IDE like VS or VS Code).
type ExecutableRunner interface {
	// Runs the Executable. When the passed context is cancelled, the run is automatically terminated.
	// Returns the run ID, and a function that enables run completion notifications delivered to the exit handler.
	StartRun(
		ctx context.Context,
		exe *apiv1.Executable,
		runCompletionHandler RunCompletionHandler,
		log logr.Logger,
	) (runID RunID, startWaitForRunCompletion func(), err error)

	// Stops the run with a given ID.
	StopRun(ctx context.Context, runID RunID, log logr.Logger) error
}

type RunCompletionHandler interface {
	// Called when the Executable run ends.
	// If err is nil, the process exit code was properly captured and the exitCode value is valid.
	// if err is not nil, there was a problem with the run and the exitCode value is not valid.
	OnRunCompleted(runID RunID, exitCode int32, err error)
}

// Make it easy to supply a function as a run completion handler.
type RunCompletionHandlerFunc func(RunID, int32, error)

func (f RunCompletionHandlerFunc) OnRunCompleted(runID RunID, exitCode int32, err error) {
	f(runID, exitCode, err)
}
