package io

import (
	"os"
	"path/filepath"
	"sync"
)

var (
	DcpTempDir func() string
)

func OpenTempFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return OpenFile(filepath.Join(DcpTempDir(), name), flag, perm)
}

func init() {
	DcpTempDir = sync.OnceValue[string](func() string {
		sessionDir := DcpSessionDir()
		if sessionDir != "" {
			return sessionDir
		} else {
			return os.TempDir()
		}
	})
}
