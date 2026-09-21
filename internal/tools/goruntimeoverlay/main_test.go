/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	dcpio "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	unpatchedRuntimeSource = `package runtime

func before() {}

type sigset uint32

var sigset_all = ^sigset(0)

//go:nosplit
//go:nowritebarrierrec
func setsig(i uint32, fn uintptr) {
	var sa usigactiont
	sa.sa_flags = _SA_SIGINFO | _SA_ONSTACK | _SA_RESTART
	sa.sa_mask = ^uint32(0)
	if fn == abi.FuncPCABIInternal(sighandler) { // abi.FuncPCABIInternal(sighandler) matches the callers in signal_unix.go
		if iscgo {
		}
	}
}

func after() {}
`
	patchedRuntimeSource = `package runtime

func before() {}

type sigset uint32

var sigset_all = ^sigset(0)

//go:nosplit
//go:nowritebarrierrec
func setsig(i uint32, fn uintptr) {
	var sa usigactiont

	sa.sa_flags = _SA_ONSTACK | _SA_RESTART
	// SA_SIGINFO should not be set when assigning SIG_DFL or SIG_IGN
	if fn != _SIG_DFL && fn != _SIG_IGN {
		sa.sa_flags |= _SA_SIGINFO
	}
	sa.sa_mask = ^uint32(0)
	if fn == abi.FuncPCABIInternal(sighandler) { // abi.FuncPCABIInternal(sighandler) matches the callers in signal_unix.go
		if iscgo {
		}
	}
}

func after() {}
`
)

func TestGenerateRuntimeOverlay(t *testing.T) {
	t.Parallel()

	goRoot := t.TempDir()
	runtimeDirectory := filepath.Join(goRoot, "src", "runtime")
	require.NoError(t, os.MkdirAll(runtimeDirectory, osutil.PermissionDirectoryOthersRead))

	runtimeSourcePath := filepath.Join(runtimeDirectory, "os_darwin.go")
	runtimeSource := []byte(unpatchedRuntimeSource)
	require.NoError(t, dcpio.WriteFile(
		runtimeSourcePath,
		runtimeSource,
		osutil.PermissionOwnerReadWriteOthersRead,
	))

	outputDirectory := filepath.Join(t.TempDir(), "overlay")
	require.NoError(t, generateRuntimeOverlay(context.Background(), goRoot, outputDirectory))

	patchedSourcePath := filepath.Join(outputDirectory, "runtime", "os_darwin.go")
	patchedSource, patchedSourceErr := readFile(patchedSourcePath)
	require.NoError(t, patchedSourceErr)
	require.Equal(t, patchedRuntimeSource, string(patchedSource))

	configContents, configReadErr := readFile(filepath.Join(outputDirectory, "overlay.json"))
	require.NoError(t, configReadErr)

	var config overlayConfig
	require.NoError(t, json.Unmarshal(configContents, &config))
	require.Equal(t, map[string]string{runtimeSourcePath: patchedSourcePath}, config.Replace)
}

func TestGenerateRuntimeOverlayIsEmptyWhenToolchainContainsFix(t *testing.T) {
	t.Parallel()

	goRoot := t.TempDir()
	runtimeDirectory := filepath.Join(goRoot, "src", "runtime")
	require.NoError(t, os.MkdirAll(runtimeDirectory, osutil.PermissionDirectoryOthersRead))

	runtimeSourcePath := filepath.Join(runtimeDirectory, "os_darwin.go")
	require.NoError(t, dcpio.WriteFile(
		runtimeSourcePath,
		[]byte(patchedRuntimeSource),
		osutil.PermissionOwnerReadWriteOthersRead,
	))

	outputDirectory := filepath.Join(t.TempDir(), "overlay")
	require.NoError(t, generateRuntimeOverlay(context.Background(), goRoot, outputDirectory))

	configContents, configReadErr := readFile(filepath.Join(outputDirectory, "overlay.json"))
	require.NoError(t, configReadErr)

	var config overlayConfig
	require.NoError(t, json.Unmarshal(configContents, &config))
	require.Empty(t, config.Replace)
}

func TestGenerateRuntimeOverlayRejectsUnexpectedSource(t *testing.T) {
	t.Parallel()

	goRoot := t.TempDir()
	runtimeDirectory := filepath.Join(goRoot, "src", "runtime")
	require.NoError(t, os.MkdirAll(runtimeDirectory, osutil.PermissionDirectoryOthersRead))

	runtimeSourcePath := filepath.Join(runtimeDirectory, "os_darwin.go")
	require.NoError(t, dcpio.WriteFile(
		runtimeSourcePath,
		[]byte("package runtime\n"),
		osutil.PermissionOwnerReadWriteOthersRead,
	))

	outputDirectory := filepath.Join(t.TempDir(), "overlay")
	generateErr := generateRuntimeOverlay(context.Background(), goRoot, outputDirectory)

	require.ErrorContains(t, generateErr, "upstream patch applies neither forward nor in reverse")
}
