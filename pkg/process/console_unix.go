//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import "github.com/go-logr/logr"

// StopViaConsole stops the process. Console attachment is Windows-specific, so this is a
// regular StopProcess call on non-Windows platforms.
func StopViaConsole(_ logr.Logger, executor Executor, handle ProcessHandle, options ...ProcessStopOption) error {
	return executor.StopProcess(handle, options...)
}
