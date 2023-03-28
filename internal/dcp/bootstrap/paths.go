package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const DcpExtensionsDir = "ext"

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
	dir, err := GetDcpDir()
	if err != nil {
		return "", err
	}

	extDir := filepath.Join(dir, DcpExtensionsDir)
	return extDir, nil
}

func GetDcpdPath() (string, error) {
	dir, err := GetDcpDir()
	if err != nil {
		return "", err
	}

	dcpdPath := ""
	if isWindows() {
		dcpdPath = filepath.Join(dir, "dcpd.exe")
	} else {
		dcpdPath = filepath.Join(dir, "dcpd")
	}

	info, err := os.Stat(dcpdPath)
	if err != nil {
		return "", fmt.Errorf("could not determine the path to the DCPD executable: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("Path '%s' points to a directory (expected DCPd executable)", dcpdPath)
	}

	return dcpdPath, nil
}

func isWindows() bool {
	return runtime.GOOS == "windows"
}
