//go:build !linux

// Copyright (c) Microsoft Corporation. All rights reserved.

package process

import (
	"time"

	ps "github.com/shirou/gopsutil/v4/process"
)

func startTimeForProcess(proc *ps.Process) time.Time {
	createTimestamp, err := proc.CreateTime()
	if err != nil {
		return time.Time{}
	}

	return time.UnixMilli(createTimestamp)
}
