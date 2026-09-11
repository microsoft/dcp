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

// InheritedSIGUSR1Ignored reports whether SIGUSR1 was ignored when this process started. It
// reports false on platforms that do not need the Darwin exec signal disposition workaround.
func InheritedSIGUSR1Ignored() (bool, error) {
	return false, nil
}

// IsSIGUSR1Ignored reports whether SIGUSR1 currently has the SIG_IGN disposition. It reports
// false on platforms that do not need the Darwin exec signal disposition workaround.
func IsSIGUSR1Ignored() (bool, error) {
	return false, nil
}

// PrepareSIGUSR1ForExec gives SIGUSR1 a disposition that is safe for an exec'd child. It is a
// no-op wherever NeedsExecSignalDispositionWorkaround reports false.
func PrepareSIGUSR1ForExec(_ bool) error {
	return nil
}
