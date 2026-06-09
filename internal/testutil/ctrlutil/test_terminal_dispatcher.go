/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/microsoft/dcp/internal/termpty"
	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

// TerminalProcessFactoryDispatcher routes TerminalProcessFactory invocations to
// per-test handlers keyed by the Executable's command path. A single dispatcher
// installs itself as the TerminalProcessFactory on the supplied
// TestProcessExecutableRunner, which lets multiple terminal tests run in
// parallel against the shared runner without stomping on each other's factory
// installation.
type TerminalProcessFactoryDispatcher struct {
	m        sync.Mutex
	handlers map[string]termpty.TerminalProcessFactory
}

// NewTerminalProcessFactoryDispatcher creates a dispatcher and installs it as
// the TerminalProcessFactory on runner.
func NewTerminalProcessFactoryDispatcher(runner *TestProcessExecutableRunner) *TerminalProcessFactoryDispatcher {
	d := &TerminalProcessFactoryDispatcher{
		handlers: map[string]termpty.TerminalProcessFactory{},
	}
	runner.SetTerminalProcessFactory(d.dispatch)
	return d
}

// dispatch looks up the handler registered for spec.Cmd.Path and invokes it.
// Returns an error if no handler is registered for the supplied command path.
func (d *TerminalProcessFactoryDispatcher) dispatch(
	ctx context.Context,
	pe process.Executor,
	spec *termpty.CommandSpec,
) (*termpty.PseudoTerminalProcess, error) {
	d.m.Lock()
	h, ok := d.handlers[spec.Cmd.Path]
	d.m.Unlock()
	if !ok {
		return nil, fmt.Errorf("no terminal handler registered for cmd %q", spec.Cmd.Path)
	}
	return h(ctx, pe, spec)
}

// InstallHandler registers handler as the TerminalProcessFactory invoked when
// an Executable with the supplied cmdPath is started. The previous handler for
// cmdPath (if any) is restored on t.Cleanup; otherwise the entry is removed.
//
// The returned channel is fed by the dispatcher every time handler returns a
// *termpty.PseudoTerminalProcess whose PTY type-asserts to a *testutil.TestPty.
// It is buffered with capacity 1 and the send is non-blocking, so the
// dispatcher never stalls the runner if the test fails to read it. Tests are
// expected to read it at most once.
func (d *TerminalProcessFactoryDispatcher) InstallHandler(
	t *testing.T,
	cmdPath string,
	handler termpty.TerminalProcessFactory,
) <-chan *testutil.TestPty {
	t.Helper()

	ptyCh := make(chan *testutil.TestPty, 1)
	wrapped := func(
		ctx context.Context,
		pe process.Executor,
		spec *termpty.CommandSpec,
	) (*termpty.PseudoTerminalProcess, error) {
		result, err := handler(ctx, pe, spec)
		if err != nil || result == nil {
			return result, err
		}
		if tp, ok := result.PTY.(*testutil.TestPty); ok {
			select {
			case ptyCh <- tp:
			default:
				// Channel buffer full — the caller is only expected to read once.
			}
		}
		return result, nil
	}

	d.m.Lock()
	prev, hadPrev := d.handlers[cmdPath]
	d.handlers[cmdPath] = wrapped
	d.m.Unlock()

	t.Cleanup(func() {
		d.m.Lock()
		defer d.m.Unlock()
		if hadPrev {
			d.handlers[cmdPath] = prev
		} else {
			delete(d.handlers, cmdPath)
		}
	})

	return ptyCh
}

// NewTestPtyTerminalFactory returns a TerminalProcessFactory that starts the
// supplied command via pe.StartProcess (so the shared TestProcessExecutor
// still sees it for SimulateProcessExit etc.) and pairs it with a freshly
// allocated *testutil.TestPty. Use it as the handler argument to
// TerminalProcessFactoryDispatcher.InstallHandler when you want a
// TestPty-backed terminal run.
func NewTestPtyTerminalFactory() termpty.TerminalProcessFactory {
	return func(
		ctx context.Context,
		pe process.Executor,
		spec *termpty.CommandSpec,
	) (*termpty.PseudoTerminalProcess, error) {
		testPty := testutil.NewTestPty()
		exitHandler := process.NewConcurrentProcessExitHandler()

		handle, startWait, startErr := pe.StartProcess(ctx, spec.Cmd, exitHandler, spec.CreationFlags, nil)
		if startErr != nil {
			_ = testPty.Close()
			return nil, startErr
		}

		return &termpty.PseudoTerminalProcess{
			PTY:              testPty,
			PID:              handle.Pid,
			IdentityTime:     handle.IdentityTime,
			StartWaitForExit: startWait,
			ExitHandler:      exitHandler,
			Executor:         pe,
		}, nil
	}
}
