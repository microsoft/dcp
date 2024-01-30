package dcppaths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DcpRootDir           = ".dcp"
	DcpExtensionsDir     = "ext"
	DcpBinDir            = "bin"
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
