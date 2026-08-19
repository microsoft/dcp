//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"syscall"
	"unsafe"
)

// Darwin's NSIG. Valid signal numbers are 1 through darwinNumSignals-1.
const darwinNumSignals = 32

// SIG_DFL, which the syscall package does not define.
const darwinSigDfl = uintptr(0)

// darwinSigactionNew mirrors Darwin's `struct __sigaction`, which is the layout the
// sigaction(2) system call expects for the new disposition. It differs from the userspace
// `struct sigaction` by the sa_tramp field that libc fills in with the signal trampoline.
// A nil trampoline is only safe when the handler is SIG_DFL or SIG_IGN, because the kernel
// stores the trampoline but never invokes it in those cases.
type darwinSigactionNew struct {
	handler uintptr
	tramp   uintptr
	mask    uint32
	flags   int32
}

// darwinSigactionOld mirrors Darwin's userspace `struct sigaction`, which is the layout the
// sigaction(2) system call uses when reporting the previous disposition.
type darwinSigactionOld struct {
	handler uintptr
	mask    uint32
	flags   int32
}

// SignalDispositionsLeakToChildren reports whether signal handler flags set by the Go runtime
// survive into an exec'd child on this platform, and therefore whether the child needs
// ResetSignalDispositions to be called on its behalf.
//
// Darwin's execve(2) resets signal handlers to SIG_DFL but preserves sa_flags. Linux clears
// sa_flags along with the handler, and Windows has no signal dispositions at all.
func SignalDispositionsLeakToChildren() bool {
	return true
}

// ResetSignalDispositions restores every catchable signal in the calling process to SIG_DFL
// with no flags and an empty mask. It must only be called by a process that is about to replace
// itself via exec, because it disables the Go runtime's own signal handling process-wide.
//
// The Go runtime installs a handler for nearly every signal at startup, and it always requests
// SA_SIGINFO|SA_ONSTACK|SA_RESTART, even where the disposition is SIG_DFL. Because Darwin's
// execve(2) preserves sa_flags, children inherit SIG_DFL together with SA_SIGINFO. Runtimes that
// read the existing disposition back before installing their own handler misinterpret that as a
// handler already being present: .NET, for example, re-registers a nil sa_sigaction and then
// jumps to address zero when it first uses SIGUSR1 to suspend threads for a garbage collection.
//
// Resetting in a process that then forks is not sufficient, because the Go runtime restores its
// own dispositions in the forked child before it reaches execve. The reset has to happen in the
// process that calls exec, which is what the 'fork-process-exec' command exists to do.
func ResetSignalDispositions() {
	act := darwinSigactionNew{
		handler: darwinSigDfl,
		tramp:   0,
		mask:    0,
		flags:   0,
	}

	for sig := 1; sig < darwinNumSignals; sig++ {
		if sig == int(syscall.SIGKILL) || sig == int(syscall.SIGSTOP) {
			// sigaction(2) rejects these with EINVAL; their disposition can never change.
			continue
		}

		// Failures are deliberately ignored: there is no useful recovery, and one signal that
		// cannot be reset must not prevent the child from being started.
		_, _, _ = syscall.Syscall(syscall.SYS_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&act)), 0)
	}
}

// signalDisposition reports the current handler and flags for a signal.
func signalDisposition(sig int) (darwinSigactionOld, error) {
	var current darwinSigactionOld
	if _, _, errno := syscall.Syscall(syscall.SYS_SIGACTION, uintptr(sig), 0, uintptr(unsafe.Pointer(&current))); errno != 0 {
		return current, errno
	}

	return current, nil
}
