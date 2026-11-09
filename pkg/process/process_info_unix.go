//go:build darwin || linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"os"
	"syscall"
)

func nativeProcessNotFound(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func readProcessInfoFromProcess(proc *os.Process, allowExited bool) (processInfo, error) {
	info, infoErr := readProcessInfo(Uint32_ToPidT(uint32(proc.Pid)))
	if infoErr != nil {
		return processInfo{}, infoErr
	}
	if info.exited && !allowExited {
		return processInfo{}, &ErrProcessNotFound{Pid: info.handle.Pid}
	}
	return info, nil
}
