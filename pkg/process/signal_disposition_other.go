//go:build !darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

// SignalDispositionsLeakToChildren reports whether signal handler flags set by the Go runtime
// survive into an exec'd child on this platform, and therefore whether the child needs
// ResetSignalDispositions to be called on its behalf.
//
// Only Darwin is affected: its execve(2) resets signal handlers to SIG_DFL but preserves
// sa_flags. Linux clears sa_flags along with the handler, and Windows has no signal dispositions
// at all.
func SignalDispositionsLeakToChildren() bool {
	return false
}

// ResetSignalDispositions restores every catchable signal in the calling process to its default
// disposition with no flags. It must only be called by a process that is about to replace itself
// via exec, because on platforms where it does something it disables the Go runtime's own signal
// handling process-wide. It is a no-op wherever SignalDispositionsLeakToChildren reports false.
func ResetSignalDispositions() {
}
