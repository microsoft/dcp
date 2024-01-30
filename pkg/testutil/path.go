package testutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

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

		cwd = filepath.Dir(cwd)
		if isRoot(cwd) || attemptNum == 100 {
			return "", fmt.Errorf("path ending with '%s' not found", filepath.Join(tailElem...))
		}
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
