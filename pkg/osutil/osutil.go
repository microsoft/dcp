// Copyright (c) Microsoft Corporation. All rights reserved.

package osutil

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

const (
	MaxCopyFileSize = 50 * 1024 * 1024 // 50MB
)

var (
	lf   = []byte("\n")
	crlf = []byte("\r\n")
)

func LF() []byte {
	return lf
}

func CRLF() []byte {
	return crlf
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}

func WithNewline(b []byte) []byte {
	// Do not modify the original slice (e.g. don't do ret = append(b, '\n'))
	var retval []byte
	if IsWindows() {
		retval = bytes.Join([][]byte{b, crlf}, nil)
	} else {
		retval = bytes.Join([][]byte{b, lf}, nil)
	}
	return retval
}

func LineSep() []byte {
	if IsWindows() {
		return crlf
	} else {
		return lf
	}
}

// Returns the full path to the current executable.
func ThisExecutablePath() (string, error) {
	const errFmt = "could not determine the path to the current executable: %w"

	ex, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf(errFmt, err)
	}

	// Try to resolve symlinks, but if that fails, just return the original path.
	// This is a best-effort attempt to get the canonical path.
	resolved, err := filepath.EvalSymlinks(ex)
	if err == nil {
		return resolved, nil
	}

	return ex, nil
}

type PathFindTarget int

const (
	FileTarget PathFindTarget = 1
	DirTarget  PathFindTarget = 2
)

// Starting from current working directory, tries to walk up the directory hierarchy
// and find the root that ends with given path to file or directory.
// Returns the root path found, or error.
func FindRootFor(target PathFindTarget, tailElem ...string) (string, error) {
	cwd, wdErr := os.Getwd()
	if wdErr != nil {
		return "", fmt.Errorf("failed to get current directory: %w", wdErr)
	}

	attemptNum := 0
	for {
		attemptNum++
		attempt := filepath.Join(append([]string{cwd}, tailElem...)...)
		file, err := os.Stat(attempt)
		if err == nil {
			if (file.IsDir() && target == DirTarget) || (!file.IsDir() && target == FileTarget) {
				return cwd, nil
			}
		}

		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("could not check for existence of path '%s': %w", attempt, err)
		}

		if isRoot(cwd) || attemptNum == 100 {
			return "", fmt.Errorf("path ending with '%s' not found", filepath.Join(tailElem...))
		}

		cwd = filepath.Dir(cwd)
	}
}

func isRoot(path string) bool {
	if path == "/" || path == "." || path == "" {
		return true
	}
	if runtime.GOOS == "windows" {
		matched, _ := regexp.MatchString(`^[a-zA-Z]:\\$`, path) // Only errors if the pattern is invalid
		return matched
	}
	return false
}
