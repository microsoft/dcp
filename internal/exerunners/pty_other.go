/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

package exerunners

import (
	"context"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

// startTerminalProcessImpl is the non-Windows fallback. The initial slice
// (Aspire 13.4) implements PTY support on Windows via ConPTY only; Linux and
// macOS support is tracked as follow-up work.
func startTerminalProcessImpl(_ context.Context, _ *apiv1.Executable) (*terminalProcess, error) {
	return nil, ErrTerminalNotSupported
}
