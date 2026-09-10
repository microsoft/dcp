//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	// Darwin's NSIG. Valid signal numbers are 1 through darwinNumSignals-1.
	darwinNumSignals = 32

	// SIG_DFL and SIG_IGN, which the syscall package does not define.
	darwinSigDfl = uintptr(0)
	darwinSigIgn = uintptr(1)
)

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

// NeedsExecSignalDispositionWorkaround reports whether an exec'd child can inherit a SIGUSR1
// disposition that is incompatible with other language runtimes.
//
// Darwin's execve(2) resets signal handlers to SIG_DFL but preserves sa_flags. Linux clears
// sa_flags along with the handler, and Windows has no signal dispositions at all.
func NeedsExecSignalDispositionWorkaround() bool {
	return true
}

// PrepareSIGUSR1ForExec gives SIGUSR1 a disposition that is safe for an exec'd child. An ignored
// disposition remains ignored; every other disposition becomes SIG_DFL with no trampoline,
// flags, or mask.
//
// Affected Go releases install handlers with SA_SIGINFO|SA_ONSTACK|SA_RESTART and restore them
// to SIG_DFL before exec without clearing those flags. Because Darwin's execve(2) preserves
// sa_flags, children can inherit SIG_DFL together with SA_SIGINFO. .NET misinterprets that as a
// handler being present and jumps to address zero when it first uses SIGUSR1 to suspend threads
// for a garbage collection.
//
// Resetting in a process that then forks is not sufficient, because the Go runtime restores its
// own dispositions in the forked child before it reaches execve. The reset has to happen in the
// process that calls exec, which is what the 'fork-process-exec' command exists to do. This
// workaround can be removed once DCP requires a Go release containing golang/go#81009.
func PrepareSIGUSR1ForExec() error {
	current, currentErr := signalDisposition(int(syscall.SIGUSR1))
	if currentErr != nil {
		return fmt.Errorf("reading SIGUSR1 disposition: %w", currentErr)
	}
	if current.handler == darwinSigIgn {
		return nil
	}

	act := darwinSigactionNew{
		handler: darwinSigDfl,
		tramp:   0,
		mask:    0,
		flags:   0,
	}

	if setErr := setSignalDisposition(int(syscall.SIGUSR1), &act); setErr != nil {
		return fmt.Errorf("resetting SIGUSR1 disposition: %w", setErr)
	}

	return nil
}

func setSignalDisposition(sig int, act *darwinSigactionNew) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(act)), 0); errno != 0 {
		return errno
	}

	return nil
}

// signalDisposition reports the current handler and flags for a signal.
func signalDisposition(sig int) (darwinSigactionOld, error) {
	var current darwinSigactionOld
	if _, _, errno := syscall.Syscall(syscall.SYS_SIGACTION, uintptr(sig), 0, uintptr(unsafe.Pointer(&current))); errno != 0 {
		return current, errno
	}

	return current, nil
}
