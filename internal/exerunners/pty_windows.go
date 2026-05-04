/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

package exerunners

import (
	"context"
	"fmt"
	"strings"

	"github.com/UserExistsError/conpty"
	apiv1 "github.com/microsoft/dcp/api/v1"
)

// conPtyAdapter wraps a *conpty.ConPty so it satisfies hmp1.PTY (specifically
// the Resize signature: cols, rows int).
type conPtyAdapter struct {
	cp *conpty.ConPty
}

func (a *conPtyAdapter) Read(p []byte) (int, error)  { return a.cp.Read(p) }
func (a *conPtyAdapter) Write(p []byte) (int, error) { return a.cp.Write(p) }
func (a *conPtyAdapter) Close() error                { return a.cp.Close() }
func (a *conPtyAdapter) Resize(cols, rows int) error { return a.cp.Resize(cols, rows) }

// startTerminalProcessImpl allocates a Windows ConPTY, spawns the executable
// attached to it, and returns a terminalProcess. The caller is responsible for
// calling pty.Close() on shutdown.
func startTerminalProcessImpl(ctx context.Context, exe *apiv1.Executable) (*terminalProcess, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, fmt.Errorf("ConPTY is not available on this Windows version: %w", conpty.ErrConPtyUnsupported)
	}

	commandLine := buildWindowsCommandLine(exe.Spec.ExecutablePath, exe.Status.EffectiveArgs)

	envBlock := make([]string, 0, len(exe.Status.EffectiveEnv))
	for _, e := range exe.Status.EffectiveEnv {
		envBlock = append(envBlock, fmt.Sprintf("%s=%s", e.Name, e.Value))
	}

	cols, rows := terminalDimensions(exe.Spec.Terminal)

	options := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(cols, rows),
		conpty.ConPtyEnv(envBlock),
	}
	if exe.Spec.WorkingDirectory != "" {
		options = append(options, conpty.ConPtyWorkDir(exe.Spec.WorkingDirectory))
	}

	cp, err := conpty.Start(commandLine, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to start process under ConPTY: %w", err)
	}

	return &terminalProcess{
		pty: &conPtyAdapter{cp: cp},
		pid: cp.Pid(),
		waitExit: func() int32 {
			exitCode, waitErr := cp.Wait(context.Background())
			if waitErr != nil {
				// Wait failures are best-effort: the connection's about to
				// close anyway. Surface UnknownExitCode to make the situation
				// visible in the terminal host.
				return -1
			}
			return int32(exitCode)
		},
	}, nil
}

// buildWindowsCommandLine constructs a single command-line string suitable for
// CreateProcessW from a path + argv. Each token is wrapped in quotes and
// embedded quotes are escaped per the documented Windows argv parsing rules.
func buildWindowsCommandLine(executablePath string, args []string) string {
	var sb strings.Builder
	sb.WriteString(quoteWindowsArg(executablePath))
	for _, a := range args {
		sb.WriteByte(' ')
		sb.WriteString(quoteWindowsArg(a))
	}
	return sb.String()
}

// quoteWindowsArg quotes a single argument per the rules CommandLineToArgvW
// uses. Empty strings, strings containing whitespace, or strings containing
// quotes get wrapped in double quotes; backslashes preceding a quote are
// doubled.
func quoteWindowsArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var sb strings.Builder
	sb.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		switch r {
		case '\\':
			backslashes++
		case '"':
			for i := 0; i < backslashes*2; i++ {
				sb.WriteByte('\\')
			}
			backslashes = 0
			sb.WriteString(`\"`)
		default:
			for i := 0; i < backslashes; i++ {
				sb.WriteByte('\\')
			}
			backslashes = 0
			sb.WriteRune(r)
		}
	}
	for i := 0; i < backslashes*2; i++ {
		sb.WriteByte('\\')
	}
	sb.WriteByte('"')
	return sb.String()
}

func terminalDimensions(t *apiv1.TerminalSpec) (cols, rows int) {
	cols, rows = 80, 24
	if t == nil {
		return
	}
	if t.Cols > 0 {
		cols = int(t.Cols)
	}
	if t.Rows > 0 {
		rows = int(t.Rows)
	}
	return
}
