//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"bytes"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// from <sys/ioccom.h>
const (
	_IOC_PARAM_SHIFT = 13
	_IOC_PARAM_MASK  = (1 << _IOC_PARAM_SHIFT) - 1
)

func _IOC_PARM_LEN(ioctl uintptr) uintptr {
	return (ioctl >> 16) & _IOC_PARAM_MASK
}

func grantpt(f *ptyMaster) error {
	return unix.IoctlSetInt(int(f.Fd()), unix.TIOCPTYGRANT, 0)
}

func unlockpt(f *ptyMaster) error {
	return unix.IoctlSetInt(int(f.Fd()), unix.TIOCPTYUNLK, 0)
}

func ptsname(f *ptyMaster) (string, error) {
	buf := make([]byte, _IOC_PARM_LEN(syscall.TIOCPTYGNAME))

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(unix.TIOCPTYGNAME),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if errno != 0 {
		return "", errno
	}

	n := bytes.IndexByte(buf[:], 0)
	if n < 0 {
		n = len(buf)
	}
	return string(buf[:n]), nil
}
