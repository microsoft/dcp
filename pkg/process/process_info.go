/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

// processInfo is a cross-platform snapshot of process identity, ancestry, and exit state.
type processInfo struct {
	handle    ProcessHandle
	parentPID Pid_t
	birth     uint64
	exited    bool
}
