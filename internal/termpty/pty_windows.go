/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

package termpty

import (
	"context"
	"fmt"
	"strings"

	"github.com/UserExistsError/conpty"
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

// startProcessImpl allocates a Windows ConPTY, spawns a process attached to
// it, and returns a Process. The caller is responsible for calling
// PTY.Close() on shutdown.
func startProcessImpl(_ context.Context, spec CommandSpec) (*Process, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, fmt.Errorf("ConPTY is not available on this Windows version: %w", conpty.ErrConPtyUnsupported)
	}

	cols, rows := normalizeDimensions(spec.Cols, spec.Rows)

	options := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(cols, rows),
	}
	if spec.Env != nil {
		options = append(options, conpty.ConPtyEnv(spec.Env))
	}
	if spec.WorkingDirectory != "" {
		options = append(options, conpty.ConPtyWorkDir(spec.WorkingDirectory))
	}

	cp, err := conpty.Start(spec.CommandLine, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to start process under ConPTY: %w", err)
	}

	return &Process{
		PTY: &conPtyAdapter{cp: cp},
		PID: cp.Pid(),
		WaitExit: func() int32 {
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

// BuildWindowsCommandLine constructs a single command-line string suitable
// for CreateProcessW from a path + argv. Each token is wrapped in quotes
// and embedded quotes are escaped per the documented Windows argv parsing
// rules.
func BuildWindowsCommandLine(executablePath string, args []string) string {
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
