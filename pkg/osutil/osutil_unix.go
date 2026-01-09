/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package osutil

import (
	"os"
)

func IsAdmin() (bool, error) {
	if os.Getuid() == 0 {
		return true, nil
	} else {
		return false, nil
	}
}
