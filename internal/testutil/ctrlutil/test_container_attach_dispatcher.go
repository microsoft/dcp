/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"testing"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/termpty"
	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

// ContainerAttachFactory mints a *termpty.PseudoTerminalProcess representing a
// terminal attach to the named test container. Mirrors
// termpty.TerminalProcessFactory but takes the container name (resolved by the
// TestContainerOrchestrator) rather than a CommandSpec.
type ContainerAttachFactory func(
	ctx context.Context,
	containerName string,
	opts containers.AttachContainerOptions,
) (*termpty.PseudoTerminalProcess, error)

// AttachedTerminalInfo carries the test-side artifacts produced by an attach
// factory: the TestPty that backs the PseudoTerminalProcess and the PID
// returned to the controller (so tests can call SimulateProcessExit on the
// shared TestProcessExecutor when they need to exercise attach-exit independent
// of container exit).
type AttachedTerminalInfo struct {
	TestPty *testutil.TestPty
	Pid     process.Pid_t
}

// ContainerAttachFactoryDispatcher routes TestContainerOrchestrator attach
// calls to per-test factories keyed by container name. A single dispatcher
// installs itself as the attach handler on the supplied
// TestContainerOrchestrator, which lets multiple terminal tests run in
// parallel against the shared orchestrator without stomping on each other's
// installation.
type ContainerAttachFactoryDispatcher struct {
	m         sync.Mutex
	factories map[string]ContainerAttachFactory
}

// NewContainerAttachFactoryDispatcher creates a dispatcher and installs it as
// the attach handler on to.
func NewContainerAttachFactoryDispatcher(to *TestContainerOrchestrator) *ContainerAttachFactoryDispatcher {
	d := &ContainerAttachFactoryDispatcher{
		factories: map[string]ContainerAttachFactory{},
	}
	to.SetContainerAttachHandler(d.dispatch)
	return d
}

// dispatch looks up the factory registered for containerName and invokes it.
// Returns an error if no factory is registered for the container.
func (d *ContainerAttachFactoryDispatcher) dispatch(
	ctx context.Context,
	containerName string,
	opts containers.AttachContainerOptions,
) (*termpty.PseudoTerminalProcess, error) {
	d.m.Lock()
	f, ok := d.factories[containerName]
	d.m.Unlock()
	if !ok {
		return nil, fmt.Errorf("no container attach factory registered for container %q", containerName)
	}
	return f(ctx, containerName, opts)
}

// InstallHandler registers factory as the ContainerAttachFactory invoked when
// AttachContainer is called for containerName. The previous factory for
// containerName (if any) is restored on t.Cleanup; otherwise the entry is
// removed.
//
// The returned channel is fed by the dispatcher every time factory returns a
// *termpty.PseudoTerminalProcess whose PTY type-asserts to a *testutil.TestPty
// (which is true for any factory built via NewTestPtyContainerAttachFactory).
// It is buffered with capacity 1 and the send is non-blocking, so the
// dispatcher never stalls the orchestrator if the test fails to read it. Tests
// are expected to read it at most once.
func (d *ContainerAttachFactoryDispatcher) InstallHandler(
	t *testing.T,
	containerName string,
	factory ContainerAttachFactory,
) <-chan *AttachedTerminalInfo {
	t.Helper()

	ch := make(chan *AttachedTerminalInfo, 1)
	wrapped := func(
		ctx context.Context,
		name string,
		opts containers.AttachContainerOptions,
	) (*termpty.PseudoTerminalProcess, error) {
		result, err := factory(ctx, name, opts)
		if err != nil || result == nil {
			return result, err
		}
		if tp, ok := result.PTY.(*testutil.TestPty); ok {
			select {
			case ch <- &AttachedTerminalInfo{TestPty: tp, Pid: result.PID}:
			default:
				// Channel buffer full — the caller is only expected to read once.
			}
		}
		return result, nil
	}

	d.m.Lock()
	prev, hadPrev := d.factories[containerName]
	d.factories[containerName] = wrapped
	d.m.Unlock()

	t.Cleanup(func() {
		d.m.Lock()
		defer d.m.Unlock()
		if hadPrev {
			d.factories[containerName] = prev
		} else {
			delete(d.factories, containerName)
		}
	})

	return ch
}

// placeholderAttachCommand returns a CLI command that the TestProcessExecutor
// can pretend to start. The command never actually runs in tests because the
// executor intercepts all process spawns and returns synthesized PIDs; the
// command path/args only need to be deterministic per-container for routing.
func placeholderAttachCommand(containerName string) *exec.Cmd {
	binary := "test-container-attach"
	if runtime.GOOS == "windows" {
		binary = "test-container-attach.exe"
	}
	return exec.Command(binary, "attach", containerName)
}

// NewTestPtyContainerAttachFactory returns a ContainerAttachFactory that
// produces a *testutil.TestPty-backed *termpty.PseudoTerminalProcess. The
// attach process is "started" via pe.StartProcess against a placeholder
// command so the shared TestProcessExecutor sees it (and exit-handler
// plumbing works the same way it does for executable terminal runs).
func NewTestPtyContainerAttachFactory(pe process.Executor) ContainerAttachFactory {
	return func(
		ctx context.Context,
		containerName string,
		_ containers.AttachContainerOptions,
	) (*termpty.PseudoTerminalProcess, error) {
		testPty := testutil.NewTestPty()
		exitHandler := process.NewConcurrentProcessExitHandler()

		handle, startWait, startErr := pe.StartProcess(
			ctx,
			placeholderAttachCommand(containerName),
			exitHandler,
			process.CreationFlagEnsureKillOnDispose,
			nil,
		)
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
