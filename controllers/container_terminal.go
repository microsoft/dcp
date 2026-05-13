/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/termpty"
)

// startContainerTerminalSession runs the container runtime CLI's `attach`
// command against an already-running container under a host PTY, then
// stands up an HMP v1 listener at spec.UDSPath that bridges viewer
// connections to that PTY. The returned Session owns the lifetime of both
// the CLI process and the listener; callers must Close it during teardown.
//
// We use `<runtime> attach` (not `<runtime> start --attach --interactive`)
// because the container is already started by the time this is called via
// the reconciler's normal `docker container start <id>` path. Running
// `docker start --attach --interactive` against a running container is a
// no-op and would leave the container's primary process with no host-side
// stdin/stdout connection.
//
// `--sig-proxy=false` prevents the attach process from forwarding signals
// (e.g. SIGINT from the dashboard) to the container; signals are delivered
// in-band via the HMP v1 input channel as keystrokes (Ctrl-C → 0x03 byte).
//
// The container must have been created with `-t -i` (allocate TTY + keep
// stdin open) for the attach to deliver a usable terminal; this is handled
// automatically when ContainerSpec.Terminal != nil && Enabled by the docker
// and podman orchestrators' applyCreateContainerOptions helper.
//
// On hosts where DCP does not yet implement PTY allocation this returns
// termpty.ErrTerminalNotSupported. With ConPTY (Windows) and creack/pty
// (Linux/macOS) implementations both present, this is currently a tautology;
// it remains as the seam if a future host platform lacks PTY support.
func startContainerTerminalSession(
	ctx context.Context,
	runner containers.CLICommandRunner,
	containerID string,
	spec *apiv1.TerminalSpec,
	log logr.Logger,
) (*termpty.Session, error) {
	if spec == nil {
		return nil, fmt.Errorf("startContainerTerminalSession: spec must be non-nil")
	}

	// Use MakeCommand to extract the configured CLI path (e.g. "docker" or
	// "podman", possibly resolved against PATH); we don't actually start the
	// command via the orchestrator's process.Executor because we need direct
	// PTY semantics (ConPTY on Windows, /dev/ptmx on Unix).
	//
	// `--detach-keys=""` disables the default Ctrl-P,Ctrl-Q detach sequence
	// so those keystrokes are forwarded into the container as plain bytes
	// (matching an interactive terminal's expectation that all keys reach
	// the application). HMP v1 viewers manage detach themselves.
	cmd := runner.MakeCommand("attach", "--sig-proxy=false", "--detach-keys=", containerID)

	startLog := log.WithValues(
		"Cmd", cmd.Path,
		"ContainerID", containerID,
		"Terminal", true,
		"UDSPath", spec.UDSPath,
	)
	startLog.Info("Attaching container under PTY...")

	// runner.MakeCommand returns an *exec.Cmd whose Path/Args were filled in
	// by exec.Command (so the runtime CLI is already PATH-resolved). Convert
	// to the argv-style termpty.CommandSpec; envs are inherited from DCP.
	tp, err := termpty.StartProcess(ctx, termpty.CommandSpec{
		Cmd:  cmd.Args,
		Dir:  cmd.Dir,
		Cols: int(spec.Cols),
		Rows: int(spec.Rows),
	})
	if err != nil {
		return nil, fmt.Errorf("starting container attach under PTY: %w", err)
	}

	session, err := termpty.StartSession(ctx, termpty.SessionConfig{
		UDSPath: spec.UDSPath,
		Cols:    int(spec.Cols),
		Rows:    int(spec.Rows),
	}, tp, startLog)
	if err != nil {
		// StartSession is responsible for closing tp.PTY when it fails.
		return nil, fmt.Errorf("starting container terminal session listener: %w", err)
	}

	return session, nil
}
