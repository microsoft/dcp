package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

const DcpRootDir = ".dcp"
const DcpExtensionsDir = "ext"

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

func GetExtensionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine the path to the user's home directory: %w", err)
	}

	return filepath.Join(home, DcpRootDir, DcpExtensionsDir), nil
}
