//go:build linux

// Copyright (c) Microsoft Corporation. All rights reserved.

package process

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
	ps "github.com/shirou/gopsutil/v4/process"
	"github.com/tklauser/go-sysconf"
)

var clockTicks int = 100 // Default to 100 if we can't get the actual value

func processIdentityTime(proc *ps.Process) time.Time {
	hostProc := osutil.EnvVarStringWithDefault("HOST_PROC", "/proc")
	stat := filepath.Join(hostProc, strconv.Itoa(int(proc.Pid)), "stat")
	contents, err := os.ReadFile(stat)
	if err != nil {
		return time.Time{}
	}

	// The 22nd field in /proc/[pid]/stat is the process start time in clock ticks since boot
	// For our purposes we can ignore the other fields and treat it as the ticks since epoch as we only need to detect changes
	// Find the end of the second field, which is the process name in parenthesis and may contain spaces or other parentheses
	// Example: 12345 (my process name) S 1 2 3 ...
	// We want to find the space after the closing parenthesis
	index := bytes.LastIndexByte(contents, ')')
	if index < 0 {
		// Malformed stat file
		return time.Time{}
	}

	// Skip over the process name and the space after it and convert the rest to fields
	fields := bytes.Fields(contents[index+2:])
	if len(fields) < 19 {
		// Malformed stat file
		return time.Time{}
	}

	// 22nd field is at index 19 (0-based) because we skipped the first two fields
	ticksStr := string(fields[19])
	ticks, err := strconv.ParseUint(ticksStr, 10, 64)
	if err != nil {
		return time.Time{}
	}

	startTimeMs := (ticks * 1000) / uint64(clockTicks)

	return time.Time{}.Add(time.Duration(startTimeMs) * time.Millisecond)
}

func init() {
	clkTck, err := sysconf.Sysconf(sysconf.SC_CLK_TCK)
	// ignore errors
	if err == nil {
		clockTicks = int(clkTck)
	}
}
