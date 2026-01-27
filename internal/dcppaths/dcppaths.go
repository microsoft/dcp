/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcppaths

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	DcpUserDir           = ".dcp"
	DcpExtensionsDir     = "ext"
	DcpBinDir            = "bin"
	DcpWorkDir           = "dcp-work"
	DcpExtensionsPathEnv = "DCP_EXTENSIONS_PATH"
	DcpBinPathEnv        = "DCP_BIN_PATH"
)

var (
	enableTestPathProbing atomic.Bool
)

func GetExtensionsDirs() ([]string, error) {
	extensionPaths := []string{}
	if extensionPath, found := os.LookupEnv(DcpExtensionsPathEnv); found {
		for _, path := range strings.Split(extensionPath, string(os.PathListSeparator)) {
			trimmed := strings.Trim(path, " ")
			if trimmed != "" {
				extensionPaths = append(extensionPaths, trimmed)
			}
		}
	}

	if len(extensionPaths) > 0 {
		return extensionPaths, nil
	}

	extensionsDir, err := probeForExtensionsDir()
	if err != nil {
		return nil, err
	}

	return []string{extensionsDir}, nil
}

func GetDcpBinDir() (string, error) {
	if binPath, found := os.LookupEnv(DcpBinPathEnv); found {
		return filepath.Abs(filepath.Clean(binPath))
	}

	exePath, err := osutil.ThisExecutablePath()
	if err == nil {
		exeDir, _ := filepath.Split(exePath)
		exeDir = filepath.Clean(exeDir)

		return exeDir, nil
	}

	cwd, cwdErr := os.Getwd()
	if cwdErr == nil {
		return filepath.Clean(cwd), nil
	}

	return "", fmt.Errorf("could not determine DCP bin directory: %w", errors.Join(err, cwdErr))
}

func WithDcpBinDir(cmd *exec.Cmd) {
	binDir, err := GetDcpBinDir()
	if err != nil {
		return
	}

	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", DcpBinPathEnv, binDir))
}

// Figure out the extensions directory based on current executable path, filesystem probing, and well-known environment variables.
func probeForExtensionsDir() (string, error) {
	// We assume the binaries can be found in 3 locations:
	// - The root directory (DCP API server ONLY),
	// - The extension directory (subfolder of the root directory), or
	// - The bin directory (subfolder of the extensions directory).

	dcpExeName := "dcp"
	if osutil.IsWindows() {
		dcpExeName += ".exe"
	}

	exePath, err := osutil.ThisExecutablePath()
	if err == nil {
		exeDir, exeName := filepath.Split(exePath)
		exeDir = filepath.Clean(exeDir)

		switch {
		case exeName == dcpExeName:
			// exeDir is the root DCP directory.
			return filepath.Join(exeDir, DcpExtensionsDir), nil

		case strings.HasSuffix(exeDir, DcpExtensionsDir):
			// exeDir is the extensions directory.
			return exeDir, nil

		case strings.HasSuffix(exeDir, filepath.Join(DcpExtensionsDir, DcpBinDir)):
			// exeDir is the bin directory.
			extensionsDir := filepath.Dir(exeDir)
			return extensionsDir, nil
		}
	}

	if enableTestPathProbing.Load() {
		tail := []string{DcpBinDir, dcpExeName}
		rootFolder, rootFindErr := osutil.FindRootFor(osutil.FileTarget, tail...)
		if rootFindErr == nil {
			return filepath.Join(rootFolder, DcpBinDir, DcpExtensionsDir), nil
		}
	}

	// Fallback: return the default DCP extensions directory inside the user's home directory.
	home, homeDirGetErr := os.UserHomeDir()
	if homeDirGetErr != nil {
		return "", fmt.Errorf("could not determine the path to DCP extensions directory: the program location is not within the standard DCP directory structure, and we could not determine the path to the user's home directory: %w", errors.Join(err, homeDirGetErr))
	}

	extensionsDir := filepath.Join(home, DcpUserDir, DcpExtensionsDir)
	return extensionsDir, nil
}

// Returns the full path to user DCP data directory, attempting to create it as necessary.
func EnsureUserDcpDir() (string, error) {
	homePath, homeDirErr := os.UserHomeDir()
	if homeDirErr != nil {
		return "", fmt.Errorf("could not obtain user home directory: %w", homeDirErr)
	}

	dcpFolder := filepath.Join(homePath, DcpUserDir)
	dcpFolderInfo, dcpFolderErr := os.Stat(dcpFolder)
	if errors.Is(dcpFolderErr, iofs.ErrNotExist) {
		if err := os.MkdirAll(dcpFolder, osutil.PermissionOnlyOwnerReadWriteTraverse); err != nil {
			return "", fmt.Errorf("failed to create DCP default directory '%s': %w", dcpFolder, err)
		}
	} else if dcpFolderErr != nil {
		return "", fmt.Errorf("failed to verify the existence of DCP default directory '%s': %w", dcpFolder, dcpFolderErr)
	} else if !dcpFolderInfo.IsDir() {
		return "", fmt.Errorf("'%s' exists, but is not a directory and cannot be used DCP default directory", dcpFolder)
	}

	absDcpFolder, absErr := filepath.Abs(dcpFolder)
	if absErr != nil {
		return "", fmt.Errorf("could not determine the absolute path to DCP default directory '%s': %w", dcpFolder, absErr)
	}

	return absDcpFolder, nil
}

// Used by tests only, enables probing for additional DCP paths during test runs.
func EnableTestPathProbing() {
	enableTestPathProbing.Store(true)
}
