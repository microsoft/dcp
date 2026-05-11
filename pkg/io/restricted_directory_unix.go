//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"fmt"
	"os"
	"syscall"
)

func validateRestrictedDirectoryOwner(_ string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("could not determine owner")
	}
	expectedUID := uint32(os.Geteuid())
	if stat.Uid != expectedUID {
		return fmt.Errorf("owner uid is %d, expected %d", stat.Uid, expectedUID)
	}
	return nil
}
