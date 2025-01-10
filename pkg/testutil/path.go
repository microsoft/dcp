package testutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
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

// Returns temporary directory root path for use in tests.
// Agents running tests in CI pipelines often require that tests use temporary directory that
// is different from what TEMP or TMPDIR environment variables point to. This function takes care of that.
func TestTempRoot() string {
	azdoTemp, found := os.LookupEnv("AGENT_TEMPDIRECTORY") // Azure DevOps pipeline
	if found {
		return azdoTemp
	}

	ghTemp, found := os.LookupEnv("RUNNER_TEMP") // GitHub Actions
	if found {
		return ghTemp
	}

	return os.TempDir()
}

// Returns a temporary directory path for use in tests.
// If session folder is set in the environment, returns that.
// Otherwise, returns the temporary directory root path.
func TestTempDir() string {
	sessionDir, found := os.LookupEnv(usvc_io.DCP_SESSION_FOLDER)
	if found {
		return sessionDir
	}

	return TestTempRoot()
}

// Creates a session directory for use in tests.
func CreateTestSessionDir() (string, error) {
	testRoot := TestTempRoot()

	suffix, randErr := randdata.MakeRandomString(8)
	if randErr != nil {
		return "", fmt.Errorf("failed to generate random suffix for session directory: %w", randErr)
	}
	dirName := fmt.Sprintf("usvc-test-%s", suffix)
	sessionDir := filepath.Join(testRoot, dirName)

	if err := os.MkdirAll(sessionDir, osutil.PermissionOnlyOwnerReadWriteSetCurrent); err != nil {
		return "", fmt.Errorf("failed to create session directory: %w", err)
	}

	return sessionDir, nil
}
