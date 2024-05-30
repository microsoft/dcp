package io

import (
	"os"
	"path/filepath"
)

const (
	DCP_SESSION_FOLDER = "DCP_SESSION_FOLDER" // Folder to delete when finished with a session
)

var (
	DcpTempDir = os.TempDir()
)

func OpenTempFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return OpenFile(filepath.Join(DcpTempDir, name), flag, perm)
}

func init() {
	if dcpSessionDir, found := os.LookupEnv(DCP_SESSION_FOLDER); found {
		DcpTempDir = dcpSessionDir
	}
}
