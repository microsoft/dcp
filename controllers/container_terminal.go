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

// startContainerTerminalSession spawns the container runtime CLI with
// `start --attach --interactive <containerID>` under a host PTY, then stands
// up an HMP v1 listener at spec.UDSPath that bridges viewer connections to
// that PTY. The returned Session owns the lifetime of both the CLI process
// and the listener; callers must Close it during teardown.
//
// The container must have been created with `-t -i` (allocate TTY + keep
// stdin open) for the attach to deliver a usable terminal; this is handled
// automatically when ContainerSpec.Terminal != nil && Enabled by the docker
// and podman orchestrators' applyCreateContainerOptions helper.
//
// On hosts where DCP does not yet implement PTY allocation (currently
// non-Windows) this returns termpty.ErrTerminalNotSupported.
func startContainerTerminalSession(
	ctx context.Context,
	runner containers.CLICommandRunner,
	containerID string,
	spec *apiv1.TerminalSpec,
	log logr.Logger,
) (*termpty.Session, error) {
	if spec == nil || !spec.Enabled {
		return nil, fmt.Errorf("startContainerTerminalSession: spec must be non-nil and Enabled")
	}

	// Use MakeCommand to extract the configured CLI path (e.g. "docker" or
	// "podman", possibly resolved against PATH); we don't actually start the
	// command via the orchestrator's process.Executor because we need direct
	// ConPTY semantics.
	cmd := runner.MakeCommand("start", "--attach", "--interactive", containerID)

	commandLine := termpty.BuildWindowsCommandLine(cmd.Path, cmd.Args[1:])

	startLog := log.WithValues(
		"Cmd", cmd.Path,
		"ContainerID", containerID,
		"Terminal", true,
		"UDSPath", spec.UDSPath,
	)
	startLog.Info("Starting container attach under PTY...")

	tp, err := termpty.StartProcess(ctx, termpty.CommandSpec{
		CommandLine: commandLine,
		Cols:        int(spec.Cols),
		Rows:        int(spec.Rows),
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
