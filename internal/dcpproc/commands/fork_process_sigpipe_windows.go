//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

// enableForkProcessSIGPIPEErrorsForProcessLifetime is a no-op on Windows.
func enableForkProcessSIGPIPEErrorsForProcessLifetime() {
}
