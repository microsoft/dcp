//go:build linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"strconv"

	"golang.org/x/sys/unix"
)

func grantpt(f *ptyMaster) error {
	// Linux automatically grants access to the slave device of a pseudo-terminal
	return nil
}

func unlockpt(f *ptyMaster) error {
	// Zero pointer clears the lock
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.TIOCSPTLCK, 0)
}

func ptsname(f *ptyMaster) (string, error) {
	n, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPTN)
	if err != nil {
		return "", err
	}
	return "/dev/pts/" + strconv.Itoa(n), nil
}
