/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
)

// startTerminalRun is the PTY-attached counterpart to the regular StartRun
// flow. It allocates a pseudo-terminal, starts the executable inside it, and
// stands up an HMP v1 listener at exe.Spec.Terminal.UDSPath.
//
// On success the returned result has ExeState=Running, Pid set, and a
// StartWaitForRunCompletion function that, when invoked, fires the eventual
// OnRunCompleted callback once the underlying process exits.
func (r *ProcessExecutableRunner) startTerminalRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) *controllers.ExecutableStartResult {
	startLog := log.WithValues("Cmd", exe.Spec.ExecutablePath, "Args", exe.Status.EffectiveArgs, "Terminal", true, "UDSPath", exe.Spec.Terminal.UDSPath)
	startLog.Info("Starting process under PTY...")

	result := controllers.NewExecutableStartResult()

	tp, err := startTerminalProcess(ctx, exe)
	if err != nil {
		startLog.Error(err, "Failed to start process under PTY")
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = err
		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
		return result
	}

	session, err := startTerminalSession(ctx, exe, tp, startLog)
	if err != nil {
		startLog.Error(err, "Failed to start terminal session listener")
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = err
		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
		return result
	}

	pid := process.Pid_t(tp.pid)
	identityTime := time.Now()
	runID := pidToRunID(pid)

	r.runningProcesses.Store(runID, &processRunState{
		identityTime:    identityTime,
		cmdInfo:         exe.Spec.ExecutablePath,
		terminalSession: session,
	})

	result.RunID = runID
	pointers.SetValue(&result.Pid, int64(pid))
	result.ExeState = apiv1.ExecutableStateRunning
	result.CompletionTimestamp = metav1.NowMicro()

	// We arm the run-completion watcher here, but it must not fire until the
	// caller has invoked StartWaitForRunCompletion (see the contract on
	// RunChangeHandler.OnStartupCompleted).
	var armed atomic.Bool
	armCh := make(chan struct{})
	result.StartWaitForRunCompletion = func() {
		if armed.CompareAndSwap(false, true) {
			close(armCh)
		}
	}

	go func() {
		// Wait until the controller is ready to accept OnRunCompleted, or
		// until the parent context is cancelled.
		select {
		case <-armCh:
		case <-ctx.Done():
			// Even if the controller never armed us, drain the session so we
			// don't leak goroutines.
			session.Close()
			<-session.Done()
			return
		}

		<-session.Done()

		// Pull the real exit code from the session, if we have one.
		var exitCode *int32
		if state, found := r.runningProcesses.LoadAndDelete(runID); found && state.terminalSession != nil {
			if code, ok := state.terminalSession.ExitCode(); ok {
				ec := code
				exitCode = &ec
			}
		}

		var runErr error
		if errors.Is(ctx.Err(), context.Canceled) {
			runErr = ctx.Err()
		}
		runChangeHandler.OnRunCompleted(runID, exitCode, runErr)
	}()

	runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
	return result
}
