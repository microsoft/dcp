/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/process"
)

func stopViaConsole(_ logr.Logger, pe process.Executor, pid process.Pid_t, startTime time.Time) error {
	return pe.StopProcess(pid, startTime)
}
