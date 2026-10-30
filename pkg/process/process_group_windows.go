//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"fmt"
)

// FindProcessGroup returns nil on Windows, where cleanup uses process trees and consoles.
func FindProcessGroup(_ ProcessHandle) (*ProcessGroup, error) {
	return nil, nil
}

func (g *ProcessGroup) isRunning(_ context.Context) (bool, error) {
	return false, fmt.Errorf("unix process group cleanup is not supported on Windows")
}

func (e *OSExecutor) stopProcessGroup(_ context.Context, _ *ProcessGroup, _ processStoppingOpts) (singleProcessStopResult, error) {
	return singleProcessStopResult{}, fmt.Errorf("unix process group cleanup is not supported on Windows")
}
