//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"debug/pe"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestLoadConPTYRejectsIncompleteBundles(t *testing.T) {
	t.Parallel()

	var processMachine, nativeMachine uint16
	require.NoError(t, windows.IsWow64Process2(windows.CurrentProcess(), &processMachine, &nativeMachine))
	hostArch, archErr := conPTYHostArchitecture(nativeMachine)
	require.NoError(t, archErr)
	hostFile := filepath.Join(hostArch, "OpenConsole.exe")

	cases := []struct {
		name       string
		files      []string
		directory  string
		errMessage string
	}{
		{name: "missing DLL", files: []string{hostFile}, errMessage: "conpty.dll"},
		{name: "missing host", files: []string{"conpty.dll"}, errMessage: "OpenConsole.exe"},
		{name: "DLL is directory", directory: "conpty.dll", errMessage: "is not a regular file"},
		{name: "host is directory", files: []string{"conpty.dll"}, directory: hostFile, errMessage: "is not a regular file"},
		{name: "invalid DLL", files: []string{"conpty.dll", hostFile}, errMessage: "could not load configured ConPTY DLL"},
		{name: "overriding host", files: []string{"conpty.dll", hostFile, "OpenConsole.exe"}, errMessage: "would override"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range tc.files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), osutil.PermissionOnlyOwnerReadWriteTraverse))
				require.NoError(t, usvc_io.WriteFile(filepath.Join(dir, name), []byte("not a binary"), osutil.PermissionOnlyOwnerReadWrite))
			}
			if tc.directory != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, tc.directory), osutil.PermissionOnlyOwnerReadWriteTraverse))
			}

			api, loadErr := loadConPTY(dir)
			require.ErrorContains(t, loadErr, tc.errMessage)
			require.Nil(t, api)
		})
	}
}

func configuredConPTY(t *testing.T) *conPTY {
	t.Helper()
	if os.Getenv(conPTYPathEnv) == "" {
		t.Skip("standalone ConPTY test requires DCP_CONPTY_PATH")
	}
	provider, loadErr := getConPTY()
	require.NoError(t, loadErr)
	api, ok := provider.(*conPTY)
	require.True(t, ok, "expected the configured standalone provider")
	return api
}

func TestConPTYFromEnvironment(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv(conPTYPathEnv, "")
		require.NoError(t, os.Unsetenv(conPTYPathEnv))
		provider, providerErr := conPTYFromEnvironment()
		require.NoError(t, providerErr)
		require.IsType(t, systemConPTY{}, provider)
	})
	t.Run("empty", func(t *testing.T) {
		t.Setenv(conPTYPathEnv, "")
		provider, providerErr := conPTYFromEnvironment()
		require.NoError(t, providerErr)
		require.IsType(t, systemConPTY{}, provider)
	})
	t.Run("invalid configuration does not fall back", func(t *testing.T) {
		t.Setenv(conPTYPathEnv, t.TempDir())
		provider, providerErr := conPTYFromEnvironment()
		require.ErrorContains(t, providerErr, "invalid DCP_CONPTY_PATH configuration")
		require.Nil(t, provider)
	})
}

func TestConPTYProcessSelection(t *testing.T) {
	const subprocessEnv = "DCP_TEST_CONPTY_SELECTION"
	if mode := os.Getenv(subprocessEnv); mode != "" {
		provider, providerErr := getConPTY()
		switch mode {
		case "invalid":
			require.ErrorContains(t, providerErr, "invalid DCP_CONPTY_PATH configuration")
			require.Nil(t, provider)
		case "system":
			require.NoError(t, providerErr)
			require.IsType(t, systemConPTY{}, provider)
		case "standalone":
			require.NoError(t, providerErr)
			require.IsType(t, &conPTY{}, provider)
		default:
			t.Fatalf("unexpected ConPTY test mode %q", mode)
		}

		t.Setenv(conPTYPathEnv, filepath.Join(t.TempDir(), "changed-after-selection"))
		cachedProvider, cachedErr := getConPTY()
		require.Equal(t, provider, cachedProvider)
		require.Equal(t, providerErr, cachedErr)
		return
	}

	t.Parallel()
	executable, executableErr := os.Executable()
	require.NoError(t, executableErr)
	cases := []struct {
		mode string
		path string
	}{
		{"system", ""},
		{"invalid", t.TempDir()},
	}
	if configuredPath := os.Getenv(conPTYPathEnv); configuredPath != "" {
		cases = append(cases, struct {
			mode string
			path string
		}{"standalone", configuredPath})
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestConPTYProcessSelection$")
			cmd.Env = append(os.Environ(), subprocessEnv+"="+tc.mode, conPTYPathEnv+"="+tc.path)
			output, runErr := cmd.CombinedOutput()
			require.NoError(t, runErr, "%s", output)
		})
	}
}

func TestConPTYLoadsConfiguredDLL(t *testing.T) {
	t.Parallel()

	api := configuredConPTY(t)
	configuredDir, dirErr := filepath.Abs(os.Getenv(conPTYPathEnv))
	require.NoError(t, dirErr)

	modulePath := make([]uint16, 32768)
	pathLen, pathErr := windows.GetModuleFileName(api.dll.Handle, &modulePath[0], uint32(len(modulePath)))
	require.NoError(t, pathErr)
	require.Less(t, pathLen, uint32(len(modulePath)))
	require.Equal(t, filepath.Join(configuredDir, "conpty.dll"), windows.UTF16ToString(modulePath[:pathLen]))

	for _, proc := range []*windows.Proc{api.create, api.resize, api.close} {
		require.Same(t, api.dll, proc.Dll)
		require.NotZero(t, proc.Addr())
	}
}

func TestConPTYReturnsHRESULT(t *testing.T) {
	t.Parallel()

	api, loadErr := getConPTY()
	require.NoError(t, loadErr)

	const invalidArgument = windows.Errno(0x80070057)
	var console windows.Handle
	createErr := api.createPseudoConsole(windows.Coord{}, windows.InvalidHandle, windows.InvalidHandle, &console)
	require.ErrorIs(t, createErr, invalidArgument)
	require.Zero(t, console)
	require.ErrorIs(t, api.resizePseudoConsole(0, windows.Coord{X: 80, Y: 24}), invalidArgument)
}

func TestConPTYDoesNotFallBackFromInvalidHost(t *testing.T) {
	t.Parallel()

	configuredAPI := configuredConPTY(t)
	dllBytes, readErr := os.ReadFile(configuredAPI.dll.Name)
	require.NoError(t, readErr)

	dir := t.TempDir()
	require.NoError(t, usvc_io.WriteFile(filepath.Join(dir, "conpty.dll"), dllBytes, osutil.PermissionOnlyOwnerReadWrite))
	hostDir := filepath.Join(dir, filepath.Base(filepath.Dir(configuredAPI.hostPath)))
	require.NoError(t, os.Mkdir(hostDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
	require.NoError(t, usvc_io.WriteFile(filepath.Join(hostDir, "OpenConsole.exe"), []byte("not an executable"), osutil.PermissionOnlyOwnerReadWrite))

	api, copyLoadErr := loadConPTY(dir)
	require.NoError(t, copyLoadErr)
	t.Cleanup(func() { require.NoError(t, api.dll.Release()) })

	var inputRead, inputWrite, outputRead, outputWrite windows.Handle
	require.NoError(t, windows.CreatePipe(&inputRead, &inputWrite, nil, 0))
	t.Cleanup(func() { require.NoError(t, closeHandles(inputRead, inputWrite)) })
	require.NoError(t, windows.CreatePipe(&outputRead, &outputWrite, nil, 0))
	t.Cleanup(func() { require.NoError(t, closeHandles(outputRead, outputWrite)) })

	var console windows.Handle
	createErr := api.createPseudoConsole(windowsConsoleSize(80, 24), inputRead, outputWrite, &console)
	if console != 0 {
		t.Cleanup(func() { api.closePseudoConsole(console) })
	}
	require.Error(t, createErr, "an unusable configured host must not fall back to Windows conhost.exe")
	require.Zero(t, console)
}

func TestConPTYHostArchitecture(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		machine uint16
		want    string
	}{
		{pe.IMAGE_FILE_MACHINE_AMD64, "x64"},
		{pe.IMAGE_FILE_MACHINE_ARM64, "arm64"},
		{pe.IMAGE_FILE_MACHINE_I386, "x86"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			arch, archErr := conPTYHostArchitecture(tc.machine)
			require.NoError(t, archErr)
			require.Equal(t, tc.want, arch)
		})
	}

	_, unsupportedErr := conPTYHostArchitecture(pe.IMAGE_FILE_MACHINE_UNKNOWN)
	require.ErrorContains(t, unsupportedErr, "unsupported ConPTY host machine type")
}

func TestPackConsoleSize(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size windows.Coord
		want uint32
	}{
		{"default", windows.Coord{X: 80, Y: 24}, 0x00180050},
		{"maximum", windows.Coord{X: 32767, Y: 32767}, 0x7fff7fff},
		{"signed", windows.Coord{X: -1, Y: -2}, 0xfffeffff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, packConsoleSize(tc.size))
		})
	}
}
