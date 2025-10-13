//go:build linux

// Copyright (c) Microsoft Corporation. All rights reserved.

package process

import (
	"time"

	ps "github.com/shirou/gopsutil/v4/process"
)

func startTimeForProcess(proc *ps.Process) time.Time {
	// FIXME: Calculation of creation time on Linux has proved unreliable, particularly in containers. Disabling for now to rely on PID alone.
	return time.Time{}
}
