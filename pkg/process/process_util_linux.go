//go:build linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/tklauser/go-sysconf"
)

var processClockTicks = sync.OnceValues(func() (uint64, error) {
	ticks, ticksErr := sysconf.Sysconf(sysconf.SC_CLK_TCK)
	if ticksErr != nil {
		return 0, fmt.Errorf("could not obtain process clock frequency: %w", ticksErr)
	}
	if ticks <= 0 {
		return 0, fmt.Errorf("invalid process clock frequency %d", ticks)
	}
	return uint64(ticks), nil
})

// HOST_PROC must describe the caller's PID namespace; it does not change syscall PID interpretation.
func processProcRoot() string {
	return osutil.EnvVarStringWithDefault("HOST_PROC", "/proc")
}

func readProcessInfo(pid Pid_t) (processInfo, error) {
	frequency, frequencyErr := processClockTicks()
	if frequencyErr != nil {
		return processInfo{}, frequencyErr
	}
	return readLinuxProcessInfo(processProcRoot(), pid, frequency)
}

func readLinuxProcessInfo(root string, pid Pid_t, frequency uint64) (processInfo, error) {
	contents, readErr := readProcFile(filepath.Join(root, strconv.FormatInt(int64(pid), 10), "stat"))
	if readErr != nil {
		return processInfo{}, processLookupError(pid, readErr)
	}
	info, parseErr := parseLinuxProcessStat(contents, frequency)
	if parseErr != nil {
		return processInfo{}, fmt.Errorf("could not parse stat for pid %d: %w", pid, parseErr)
	}
	if info.handle.Pid != pid {
		return processInfo{}, fmt.Errorf("stat pid %d does not match requested pid %d", info.handle.Pid, pid)
	}
	return info, nil
}

func parseLinuxProcessStat(contents []byte, frequency uint64) (processInfo, error) {
	nameStart := bytes.IndexByte(contents, '(')
	nameEnd := bytes.LastIndexByte(contents, ')')
	if nameStart <= 0 || nameEnd < nameStart || nameEnd+1 >= len(contents) || contents[nameEnd+1] != ' ' {
		return processInfo{}, fmt.Errorf("invalid process name boundary")
	}
	pid, pidErr := StringToPidT(string(bytes.TrimSpace(contents[:nameStart])))
	if pidErr != nil || pid == 0 {
		return processInfo{}, fmt.Errorf("invalid stat pid %q", contents[:nameStart])
	}
	fields := bytes.Fields(contents[nameEnd+1:])
	if len(fields) < 20 {
		return processInfo{}, fmt.Errorf("stat has %d fields after the process name, expected at least 20", len(fields))
	}
	parentPID, parentErr := StringToPidT(string(fields[1]))
	if parentErr != nil {
		return processInfo{}, fmt.Errorf("invalid parent pid: %w", parentErr)
	}
	ticks, ticksErr := strconv.ParseUint(string(fields[19]), 10, 64)
	if ticksErr != nil {
		return processInfo{}, fmt.Errorf("invalid start ticks: %w", ticksErr)
	}
	identityTime, identityErr := linuxIdentityTime(ticks, frequency)
	if identityErr != nil {
		return processInfo{}, identityErr
	}
	return processInfo{
		handle:    NewHandle(pid, identityTime),
		parentPID: parentPID,
		birth:     ticks,
		exited:    string(fields[0]) == "Z" || string(fields[0]) == "X" || string(fields[0]) == "x",
	}, nil
}

func linuxIdentityTime(ticks uint64, frequency uint64) (time.Time, error) {
	if frequency == 0 {
		return time.Time{}, fmt.Errorf("process clock frequency is zero")
	}
	const maxMilliseconds = uint64(math.MaxInt64 / int64(time.Millisecond))
	seconds := ticks / frequency
	if seconds > maxMilliseconds/1000 {
		return time.Time{}, fmt.Errorf("process start ticks exceed supported duration")
	}
	high, low := bits.Mul64(ticks%frequency, 1000)
	fraction, _ := bits.Div64(high, low, frequency)
	milliseconds := seconds*1000 + fraction
	if milliseconds > maxMilliseconds {
		return time.Time{}, fmt.Errorf("%w: invalid process start duration", ErrProcessIdentityUnavailable)
	}
	return time.Time{}.Add(time.Duration(milliseconds) * time.Millisecond), nil
}

func snapshotProcesses(ctx context.Context) ([]processInfo, error) {
	frequency, frequencyErr := processClockTicks()
	if frequencyErr != nil {
		return nil, frequencyErr
	}
	return snapshotLinuxProcesses(ctx, processProcRoot(), frequency)
}

func snapshotLinuxProcesses(ctx context.Context, root string, frequency uint64) ([]processInfo, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	directory, openErr := usvc_io.OpenFileReadOnly(root)
	if openErr != nil {
		return nil, fmt.Errorf("could not open process directory: %w", openErr)
	}
	entries, entriesErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if directoryErr := errors.Join(entriesErr, closeErr); directoryErr != nil {
		return nil, fmt.Errorf("could not read process directory: %w", directoryErr)
	}

	processes := make([]processInfo, 0, len(entries))
	var inspectionErrors []error
	for _, entry := range entries {
		if contextErr := ctx.Err(); contextErr != nil {
			return processes, errors.Join(append(inspectionErrors, contextErr)...)
		}
		if !entry.IsDir() {
			continue
		}
		pid, pidErr := StringToPidT(entry.Name())
		if pidErr != nil || pid == 0 {
			continue
		}
		info, infoErr := readLinuxProcessInfo(root, pid, frequency)
		if infoErr != nil {
			if !IsProcessGoneErr(infoErr) {
				inspectionErrors = append(inspectionErrors, infoErr)
			}
			continue
		}
		processes = append(processes, info)
	}
	return processes, errors.Join(inspectionErrors...)
}

func processDisplayTime(info processInfo) (time.Time, error) {
	contents, readErr := readProcFile(filepath.Join(processProcRoot(), "stat"))
	if readErr != nil {
		return time.Time{}, fmt.Errorf("could not read boot time: %w", readErr)
	}
	for line := range bytes.Lines(contents) {
		fields := bytes.Fields(line)
		if len(fields) != 2 || string(fields[0]) != "btime" {
			continue
		}
		bootSeconds, bootErr := strconv.ParseInt(string(fields[1]), 10, 64)
		if bootErr != nil || bootSeconds < 0 {
			return time.Time{}, fmt.Errorf("invalid boot time %q", fields[1])
		}
		return time.Unix(bootSeconds, 0).UTC().Add(info.handle.IdentityTime.Sub(time.Time{})), nil
	}
	return time.Time{}, fmt.Errorf("boot time is missing from procfs stat")
}

func readProcFile(name string) ([]byte, error) {
	file, openErr := usvc_io.OpenFileReadOnly(name)
	if openErr != nil {
		return nil, openErr
	}
	contents, readErr := io.ReadAll(file)
	return contents, errors.Join(readErr, file.Close())
}

func formatIdentityTime(identityTime time.Time) string {
	return fmt.Sprintf("%dms-since-boot", identityTime.Sub(time.Time{})/time.Millisecond)
}
