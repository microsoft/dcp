//go:build !linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
	ps "github.com/shirou/gopsutil/v4/process"
)

func processIdentityTime(proc *ps.Process) time.Time {
	createTimestamp, err := proc.CreateTime()
	if err != nil {
		return time.Time{}
	}

	return time.UnixMilli(createTimestamp)
}

func formatIdentityTime(identityTime time.Time) string {
	return identityTime.Format(osutil.RFC3339MiliTimestampFormat)
}
