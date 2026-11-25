//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"bytes"
	"context"
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/sys/unix"

	"github.com/microsoft/dcp/pkg/resiliency"
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

// XNU's ptmx_get_ioctl/ptcopen returns EREDRIVEOPEN (-6) when concurrent
// opens select the same minor number.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/tty_ptmx.c
const darwinRedriveOpen = ^syscall.Errno(5)

func openPTYMaster(ctx context.Context, openMaster func() (*os.File, error)) (*os.File, error) {
	// Jittered exponential delays desynchronize callers that keep colliding on the same minor number.
	policy := backoff.WithMaxRetries(backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(time.Millisecond),
		backoff.WithMultiplier(2),
		backoff.WithRandomizationFactor(0.5),
		backoff.WithMaxInterval(16*time.Millisecond),
		backoff.WithMaxElapsedTime(0),
	), 9)
	return redrivePTYMasterOpen(ctx, policy, openMaster)
}

// Retries only the master open, not the later ioctls, while the open fails with EREDRIVEOPEN.
func redrivePTYMasterOpen(ctx context.Context, policy backoff.BackOff, openMaster func() (*os.File, error)) (*os.File, error) {
	return resiliency.RetryGet(ctx, policy, func() (*os.File, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, resiliency.Permanent(ctxErr)
		}
		file, openErr := openMaster()
		if ctxErr := ctx.Err(); openErr != nil && ctxErr != nil {
			return nil, resiliency.Permanent(errors.Join(openErr, ctxErr))
		}
		if openErr != nil && !errors.Is(openErr, darwinRedriveOpen) {
			return nil, resiliency.Permanent(openErr)
		}
		return file, openErr
	})
}
