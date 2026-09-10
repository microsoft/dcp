//go:build !darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

// NeedsExecSignalDispositionWorkaround reports whether an exec'd child can inherit a SIGUSR1
// disposition that is incompatible with other language runtimes.
//
// Only Darwin is affected: its execve(2) resets signal handlers to SIG_DFL but preserves
// sa_flags. Linux clears sa_flags along with the handler, and Windows has no signal dispositions
// at all.
func NeedsExecSignalDispositionWorkaround() bool {
	return false
}

// PrepareSIGUSR1ForExec gives SIGUSR1 a disposition that is safe for an exec'd child. It is a
// no-op wherever NeedsExecSignalDispositionWorkaround reports false.
func PrepareSIGUSR1ForExec() error {
	return nil
}
