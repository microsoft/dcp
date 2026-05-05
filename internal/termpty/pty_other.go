/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

package termpty

import (
	"context"
)

// startProcessImpl is the non-Windows fallback. The initial slice
// (Aspire 13.4) implements PTY support on Windows via ConPTY only; Linux and
// macOS support is tracked as follow-up work.
func startProcessImpl(_ context.Context, _ CommandSpec) (*Process, error) {
	return nil, ErrTerminalNotSupported
}

// BuildWindowsCommandLine is a no-op stub on non-Windows platforms; it lets
// callers reference it unconditionally without a build tag. Returns the
// empty string so accidental use surfaces obviously.
func BuildWindowsCommandLine(_ string, _ []string) string {
	return ""
}
