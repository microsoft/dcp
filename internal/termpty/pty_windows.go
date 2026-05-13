//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"fmt"
	"strings"

	"github.com/microsoft/dcp/pkg/process"
)

// startProcessImpl allocates a Windows ConPTY, spawns a process attached to
// it, and returns a Process. The caller is responsible for calling
// PTY.Close() on shutdown.
func startProcessImpl(_ context.Context, spec CommandSpec) (*PseudoTerminalProcess, error) {
	if len(spec.Cmd) == 0 {
		return nil, fmt.Errorf("command is empty")
	}

	cols, rows := normalizeDimensions(spec.Cols, spec.Rows)
	commandLine := BuildWindowsCommandLine(spec.Cmd[0], spec.Cmd[1:])

	cp, startErr := startWindowsPseudoConsole(windowsPseudoConsoleConfig{
		CommandLine: commandLine,
		Env:         envMapToSlice(spec.Env),
		Dir:         spec.Dir,
		Cols:        cols,
		Rows:        rows,
	})
	if startErr != nil {
		return nil, fmt.Errorf("failed to start process under ConPTY: %w", startErr)
	}

	pid := cp.processID()
	return &PseudoTerminalProcess{
		PTY:          cp,
		PID:          pid,
		IdentityTime: process.ProcessIdentityTime(pid),

		WaitExit: func(ctx context.Context) int32 {
			exitCode, waitErr := cp.wait(ctx)
			if waitErr != nil {
				// Wait failures are best-effort: the connection's about to
				// close anyway. Surface UnknownExitCode to make the situation
				// visible in the terminal host.
				return process.UnknownExitCode
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
