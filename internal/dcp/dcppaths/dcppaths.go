package dcppaths

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

const (
	DcpRootDir           = ".dcp"
	DcpExtensionsDir     = "ext"
	DcpBinDir            = "bin"
	DcpWorkDir           = "dcp-work"
	DcpExtensionsPathEnv = "DCP_EXTENSIONS_PATH"
	DcpBinPathEnv        = "DCP_BIN_PATH"
)

// Get path to current DCP CLI executable
func GetDcpDir() (string, error) {
	const errFmt = "could not determine the path to the DCP CLI executable: %w"

	ex, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf(errFmt, err)
	}

	ex, err = filepath.EvalSymlinks(ex)
	if err != nil {
		return "", fmt.Errorf(errFmt, err)
	}

	dir := filepath.Dir(ex)
	return dir, nil
}

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

	if len(extensionPaths) == 0 {
		dcpDir, err := GetDcpDir()
		if err != nil {
			home, homeDirGetErr := os.UserHomeDir()
			if homeDirGetErr == nil {
				dcpDir = filepath.Join(home, DcpRootDir)
			} else {
				return nil, fmt.Errorf("could not determine the path to the user's home directory: %w", homeDirGetErr)
			}
		}

		extensionPaths = []string{filepath.Join(dcpDir, DcpExtensionsDir)}
	}

	return extensionPaths, nil
}

func GetDcpBinDir() (string, error) {
	if binPath, found := os.LookupEnv(DcpBinPathEnv); found {
		return binPath, nil
	}

	dcpDir, err := GetDcpDir()
	if err != nil {
		home, homeDirGetErr := os.UserHomeDir()
		if homeDirGetErr == nil {
			dcpDir = filepath.Join(home, DcpRootDir)
		} else {
			return "", fmt.Errorf("could not determine the path to the user's home directory: %w", homeDirGetErr)
		}
	}

	return filepath.Join(dcpDir, DcpBinDir), nil
}

// Returns the full path to DCP root directory, attempting to create it as necessary.
func EnsureDcpRootDir() (string, error) {
	homePath, homeDirErr := os.UserHomeDir()
	if homeDirErr != nil {
		return "", fmt.Errorf("could not obtain user home directory: %w", homeDirErr)
	}

	dcpFolder := filepath.Join(homePath, DcpRootDir)
	dcpFolderInfo, dcpFolderErr := os.Stat(dcpFolder)
	if errors.Is(dcpFolderErr, iofs.ErrNotExist) {
		if err := os.MkdirAll(dcpFolder, osutil.PermissionOnlyOwnerReadWriteTraverse); err != nil {
			return "", fmt.Errorf("failed to create DCP default directory '%s': %w", dcpFolder, err)
		}
	} else if dcpFolderErr != nil {
		return "", fmt.Errorf("failed to verify the existence of DCP  default directory '%s': %w", dcpFolder, dcpFolderErr)
	} else if !dcpFolderInfo.IsDir() {
		return "", fmt.Errorf("'%s' exists, but is not a directory and cannot be used DCP default directory", dcpFolder)
	}

	absDcpFolder, absErr := filepath.Abs(dcpFolder)
	if absErr != nil {
		return "", fmt.Errorf("could not determine the absolute path to DCP default directory '%s': %w", dcpFolder, absErr)
	}

	return absDcpFolder, nil
}
