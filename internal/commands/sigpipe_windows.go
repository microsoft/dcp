//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

// handleSigpipeSignalsForProcessLifetime is a no-op on Windows, which does not use SIGPIPE for broken pipes.
func handleSigpipeSignalsForProcessLifetime() {
}
