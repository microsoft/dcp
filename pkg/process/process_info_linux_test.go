//go:build linux

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/stretchr/testify/require"
)

func linuxStatFixture(pid, parent int, name, ticks string) []byte {
	fields := make([]string, 20)
	for index := range fields {
		fields[index] = "0"
	}
	fields[0] = "S"
	fields[1] = strconv.Itoa(parent)
	fields[19] = ticks
	return fmt.Appendf(nil, "%d (%s) %s\n", pid, name, strings.Join(fields, " "))
}

func TestParseLinuxProcessStat(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"simple", "a name", "a ) strange (name)", "line\nbreak"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			info, parseErr := parseLinuxProcessStat(linuxStatFixture(12, 10, name, "12345"), 100)
			require.NoError(t, parseErr)
			require.Equal(t, Pid_t(12), info.handle.Pid)
			require.Equal(t, Pid_t(10), info.parentPID)
			require.Equal(t, uint64(12345), info.birth)
			require.Equal(t, time.Time{}.Add(123450*time.Millisecond), info.handle.IdentityTime)
		})
	}
}

func TestParseLinuxProcessStatRejectsMalformedRecords(t *testing.T) {
	t.Parallel()
	valid := string(linuxStatFixture(12, 10, "name", "12345"))
	tests := []string{
		"", "12 (name)", "12 (name)S", "12 name S",
		strings.Replace(valid, "12 (", "invalid (", 1),
		strings.Replace(valid, "S 10", "S invalid", 1),
		strings.Replace(valid, "12345", "-1", 1),
		strings.Replace(valid, "12345", "invalid", 1),
		strings.Replace(valid, "12345", strconv.FormatUint(math.MaxUint64, 10), 1),
		strings.TrimSuffix(valid, "12345\n"),
	}
	for index, contents := range tests {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			_, parseErr := parseLinuxProcessStat([]byte(contents), 100)
			require.Error(t, parseErr)
		})
	}
	_, frequencyErr := parseLinuxProcessStat([]byte(valid), 0)
	require.Error(t, frequencyErr)
	zeroInfo, zeroErr := parseLinuxProcessStat(linuxStatFixture(1, 0, "init", "0"), 100)
	require.NoError(t, zeroErr)
	require.True(t, zeroInfo.handle.IdentityTime.IsZero(), "unidentifiable kernel records must not become valid handles")
}

func TestLinuxSnapshotUsesSuppliedProcRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, pid := range []int{10, 11} {
		directory := filepath.Join(root, strconv.Itoa(pid))
		require.NoError(t, os.Mkdir(directory, 0755))
		require.NoError(t, usvc_io.WriteFile(filepath.Join(directory, "stat"), linuxStatFixture(pid, 10, "child", "10000"), 0600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, "self"), 0755))
	records, snapshotErr := snapshotLinuxProcesses(context.Background(), root, 100)
	require.NoError(t, snapshotErr)
	require.Len(t, records, 2)
	for _, info := range records {
		require.Equal(t, time.Time{}.Add(100*time.Second), info.handle.IdentityTime)
	}
	require.NoError(t, usvc_io.WriteFile(filepath.Join(root, "11", "stat"), []byte("malformed"), 0600))
	partial, partialErr := snapshotLinuxProcesses(context.Background(), root, 100)
	require.Error(t, partialErr)
	require.Len(t, partial, 1)
	_, missingErr := readLinuxProcessInfo(root, 99, 100)
	require.True(t, IsProcessGoneErr(missingErr))
	_, wrongPIDErr := readLinuxProcessInfo(filepath.Join(root, "missing"), 10, 100)
	require.True(t, IsProcessGoneErr(wrongPIDErr))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cancellationErr := snapshotLinuxProcesses(ctx, root, 100)
	require.ErrorIs(t, cancellationErr, context.Canceled)
}

func TestLinuxIdentityConversionLimits(t *testing.T) {
	t.Parallel()
	_, overflowErr := linuxIdentityTime(math.MaxUint64, 1)
	require.Error(t, overflowErr)
	identity, conversionErr := linuxIdentityTime(math.MaxUint64-1, math.MaxUint64)
	require.NoError(t, conversionErr)
	require.Equal(t, time.Time{}.Add(999*time.Millisecond), identity)
}
