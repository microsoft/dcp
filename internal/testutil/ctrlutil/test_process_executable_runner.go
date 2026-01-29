/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/exerunners"
	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
)

// TestProcessExecutableRunner wraps an ExecutableRunner and allows tests to simulate
// asynchronous executable startup by delaying the call to the underlying runner.
type TestProcessExecutableRunner struct {
	inner          controllers.ExecutableRunner
	autoExecutions []testutil.AutoExecution
	m              *sync.RWMutex
}

func NewTestProcessExecutableRunner(pe process.Executor) *TestProcessExecutableRunner {
	return &TestProcessExecutableRunner{
		inner: exerunners.NewProcessExecutableRunner(pe),
		m:     &sync.RWMutex{},
	}
}

// InstallAsyncStartConfig adds an async start configuration.
// If a configuration with the same condition already exists, it will be replaced.
func (r *TestProcessExecutableRunner) InstallAutoExecution(ae testutil.AutoExecution) {
	r.m.Lock()
	defer r.m.Unlock()

	// Remove any previous config that matches the same criteria.
	withoutExisting := slices.Select(r.autoExecutions, func(existing testutil.AutoExecution) bool {
		return !ae.Condition.Equals(&existing.Condition)
	})

	r.autoExecutions = append(withoutExisting, ae)
}

func (e *TestProcessExecutableRunner) RemoveAutoExecution(sc testutil.ProcessSearchCriteria) {
	e.m.Lock()
	defer e.m.Unlock()

	e.autoExecutions = slices.Select(e.autoExecutions, func(ae testutil.AutoExecution) bool {
		return !sc.Equals(&ae.Condition)
	})
}

func (r *TestProcessExecutableRunner) StartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) *controllers.ExecutableStartResult {
	r.m.RLock()
	cmd := exec.Command(exe.Spec.ExecutablePath)
	cmd.Args = append([]string{exe.Spec.ExecutablePath}, exe.Status.EffectiveArgs...)
	// Executable spec has other properties (env, working dir) but they are not needed for matching criteria.
	i := slices.IndexFunc(r.autoExecutions, func(ae testutil.AutoExecution) bool { return ae.Condition.MatchesCmd(cmd) })
	var ae *testutil.AutoExecution
	if i != -1 {
		ae = &r.autoExecutions[i]
	}
	r.m.RUnlock()

	if ae == nil || ae.AsynchronousStartupDelay == 0 {
		return r.inner.StartRun(ctx, exe, runChangeHandler, log)
	}

	// Start a goroutine to call the underlying runner after the delay.
	go func() {
		delay := ae.AsynchronousStartupDelay

		select {
		case <-ctx.Done():
			return // Time out before delay elapsed.
		case <-time.After(delay):
			// Proceed to call the underlying runner.
		}

		// Call the underlying runner. It will call OnStartupCompleted() on the runChangeHandler.
		// We don't need to do anything with the result here--it will be reported by the underlying runner
		// via OnStartupCompleted().
		_ = r.inner.StartRun(ctx, exe, runChangeHandler, log)
	}()

	result := controllers.NewExecutableStartResult()
	result.ExeState = apiv1.ExecutableStateStarting
	return result
}

// StopRun implements ExecutableRunner by delegating to the underlying runner.
func (r *TestProcessExecutableRunner) StopRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	return r.inner.StopRun(ctx, runID, log)
}

var _ controllers.ExecutableRunner = (*TestProcessExecutableRunner)(nil)
