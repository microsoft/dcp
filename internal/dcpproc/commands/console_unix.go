/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/process"
)

func attachToTargetProcessConsole(log logr.Logger, targetPid process.Pid_t) error {
	return nil // No-op on non-Windows platforms.
}
