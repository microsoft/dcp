//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package osutil

import (
	"os"
)

// MaxUnixSocketPathLen is a conservative upper bound on the length of a Unix
// domain socket path. The kernel sun_path buffer is 108 bytes on Linux and 104
// on macOS/BSD; we use the smaller limit minus one for the null terminator.
const MaxUnixSocketPathLen = 103

func IsAdmin() (bool, error) {
	if os.Getuid() == 0 {
		return true, nil
	} else {
		return false, nil
	}
}
