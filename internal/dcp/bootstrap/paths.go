package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DcpRootDir = ".dcp"
const DcpExtensionsDir = "ext"
const DcpExtensionsPathEnv = "DCP_EXTENSIONS_PATH"

// Get path to DCP CLI executable
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
			path := strings.Trim(path, " ")
			if path != "" {
				extensionPaths = append(extensionPaths, path)
			}
		}
	}

	if len(extensionPaths) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("could not determine the path to the user's home directory: %w", err)
		}

		extensionPaths = []string{filepath.Join(home, DcpRootDir, DcpExtensionsDir)}
	}

	return extensionPaths, nil
}
