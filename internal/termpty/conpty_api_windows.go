//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"debug/pe"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/microsoft/dcp/internal/dcppaths"
)

type conPTY struct {
	dll      *windows.DLL
	create   *windows.Proc
	resize   *windows.Proc
	close    *windows.Proc
	hostPath string
}

// Keep the DLL loaded for the process lifetime so all HPCONs use the same implementation.
var getConPTY = sync.OnceValues(func() (*conPTY, error) {
	dcpDir, dirErr := dcppaths.GetDcpDir()
	if dirErr != nil {
		return nil, fmt.Errorf("could not locate bundled ConPTY: %w", dirErr)
	}
	return loadConPTY(dcpDir)
})

func loadConPTY(directory string) (*conPTY, error) {
	absoluteDir, pathErr := filepath.Abs(directory)
	if pathErr != nil {
		return nil, fmt.Errorf("could not resolve ConPTY directory: %w", pathErr)
	}

	var processMachine, nativeMachine uint16
	machineErr := windows.IsWow64Process2(windows.CurrentProcess(), &processMachine, &nativeMachine)
	if machineErr != nil {
		return nil, fmt.Errorf("could not determine ConPTY host architecture: %w", machineErr)
	}
	hostArch, archErr := conPTYHostArchitecture(nativeMachine)
	if archErr != nil {
		return nil, archErr
	}

	dllPath := filepath.Join(absoluteDir, "conpty.dll")
	dllErr := requireConPTYBinary(dllPath)
	if dllErr != nil {
		return nil, dllErr
	}
	hostPath := filepath.Join(absoluteDir, hostArch, "OpenConsole.exe")
	hostErr := validateConPTYHost(hostPath)
	if hostErr != nil {
		return nil, hostErr
	}

	module, loadErr := windows.LoadLibraryEx(dllPath, 0,
		windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if loadErr != nil {
		return nil, fmt.Errorf("could not load bundled ConPTY DLL %q: %w", dllPath, loadErr)
	}

	api := &conPTY{dll: &windows.DLL{Name: dllPath, Handle: module}, hostPath: hostPath}
	for _, binding := range []struct {
		name string
		proc **windows.Proc
	}{
		{"ConptyCreatePseudoConsole", &api.create},
		{"ConptyResizePseudoConsole", &api.resize},
		{"ConptyClosePseudoConsole", &api.close},
	} {
		proc, findErr := api.dll.FindProc(binding.name)
		if findErr != nil {
			releaseErr := api.dll.Release()
			return nil, fmt.Errorf("could not bind bundled ConPTY function %s: %w",
				binding.name, errors.Join(findErr, releaseErr))
		}
		*binding.proc = proc
	}

	return api, nil
}

func (api *conPTY) createPseudoConsole(size windows.Coord, input, output windows.Handle, console *windows.Handle) error {
	hostErr := validateConPTYHost(api.hostPath)
	if hostErr != nil {
		return hostErr
	}

	result, _, _ := api.create.Call(
		uintptr(packConsoleSize(size)),
		uintptr(input),
		uintptr(output),
		0,
		uintptr(unsafe.Pointer(console)),
	)
	// These APIs return HRESULT, not GetLastError.
	if int32(result) < 0 {
		return windows.Errno(uint32(result))
	}
	return nil
}

func conPTYHostArchitecture(nativeMachine uint16) (string, error) {
	// The host must match Windows, not DCP's architecture when running under emulation.
	switch nativeMachine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "x64", nil
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64", nil
	case pe.IMAGE_FILE_MACHINE_I386:
		return "x86", nil
	default:
		return "", fmt.Errorf("unsupported ConPTY host machine type 0x%04x", nativeMachine)
	}
}

func validateConPTYHost(hostPath string) error {
	// An adjacent host overrides the native-architecture subdirectory in conpty.dll.
	adjacentPath := filepath.Join(filepath.Dir(filepath.Dir(hostPath)), "OpenConsole.exe")
	_, adjacentErr := os.Stat(adjacentPath)
	if adjacentErr == nil {
		return fmt.Errorf("unexpected ConPTY host %q would override the native-architecture host %q", adjacentPath, hostPath)
	}
	if !errors.Is(adjacentErr, os.ErrNotExist) {
		return fmt.Errorf("could not check for an overriding ConPTY host %q: %w", adjacentPath, adjacentErr)
	}

	// conpty.dll silently uses the OS console host if the bundled host is missing.
	return requireConPTYBinary(hostPath)
}

func requireConPTYBinary(path string) error {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return fmt.Errorf("required ConPTY binary %q is unavailable: %w", path, statErr)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("required ConPTY binary %q is not a regular file", path)
	}
	return nil
}

func (api *conPTY) resizePseudoConsole(console windows.Handle, size windows.Coord) error {
	result, _, _ := api.resize.Call(uintptr(console), uintptr(packConsoleSize(size)))
	if int32(result) < 0 {
		return windows.Errno(uint32(result))
	}
	return nil
}

func (api *conPTY) closePseudoConsole(console windows.Handle) {
	_, _, _ = api.close.Call(uintptr(console))
}

func packConsoleSize(size windows.Coord) uint32 {
	// COORD is passed by value as two packed 16-bit integers.
	return uint32(uint16(size.X)) | uint32(uint16(size.Y))<<16
}
