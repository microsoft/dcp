// Copyright (c) Microsoft Corporation. All rights reserved.

package lockfile_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/lockfile"
	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

var (
	log = logger.New("lockfile-tests").Logger
)

// Create a new Lockfile, lock it, write some to it, unlock.
// Lock it again and verify the data can be read back.
func TestLockfileWriteRead(t *testing.T) {
	t.Parallel()

	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), t.Name()+".lockfile")
	defer func() {
		_ = os.Remove(path) // Best effort cleanup
	}()
	lf, err := lockfile.NewLockfile(path)
	require.NoError(t, err)

	lockErr := lf.TryLock(testCtx, lockfile.DefaultLockRetryInterval)
	require.NoError(t, lockErr)

	_, writeErr := io.WriteString(lf, "Hello, World!")
	require.NoError(t, writeErr)

	unlockErr := lf.Unlock()
	require.NoError(t, unlockErr)

	lockErr = lf.TryLock(testCtx, lockfile.DefaultLockRetryInterval)
	require.NoError(t, lockErr)

	_, seekErr := lf.Seek(0, io.SeekStart)
	require.NoError(t, seekErr)

	content, readErr := io.ReadAll(lf)
	require.NoError(t, readErr)

	require.Equal(t, "Hello, World!", string(content))

	closeErr := lf.Close()
	require.NoError(t, closeErr)
}

// Create a new Lockfile, lock it, write some data to it, unlock.
// Lock it again and verify the data can be read back.
// Lock it again, truncate, write different data, unlock.
// Lock it again and verify the new data can be read back.
func TestLockfileWriteReadTruncate(t *testing.T) {
	t.Parallel()

	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), t.Name()+".lockfile")
	defer func() {
		_ = os.Remove(path) // Best effort cleanup
	}()
	lf, err := lockfile.NewLockfile(path)
	require.NoError(t, err)

	lockErr := lf.TryLock(testCtx, lockfile.DefaultLockRetryInterval)
	require.NoError(t, lockErr)

	_, writeErr := io.WriteString(lf, "Hello, World!")
	require.NoError(t, writeErr)

	unlockErr := lf.Unlock()
	require.NoError(t, unlockErr)

	lockErr = lf.TryLock(testCtx, lockfile.DefaultLockRetryInterval)
	require.NoError(t, lockErr)

	_, seekErr := lf.Seek(0, io.SeekStart)
	require.NoError(t, seekErr)

	content, readErr := io.ReadAll(lf)
	require.NoError(t, readErr)

	require.Equal(t, "Hello, World!", string(content))

	lockErr = lf.TryLock(testCtx, lockfile.DefaultLockRetryInterval)
	require.NoError(t, lockErr)

	truncateErr := lf.Truncate(0)
	require.NoError(t, truncateErr)

	_, seekErr = lf.Seek(0, io.SeekStart)
	require.NoError(t, seekErr)

	_, writeErr = io.WriteString(lf, "Goodbye, World!")
	require.NoError(t, writeErr)

	unlockErr = lf.Unlock()
	require.NoError(t, unlockErr)

	lockErr = lf.TryLock(testCtx, lockfile.DefaultLockRetryInterval)
	require.NoError(t, lockErr)

	_, seekErr = lf.Seek(0, io.SeekStart)
	require.NoError(t, seekErr)

	content, readErr = io.ReadAll(lf)
	require.NoError(t, readErr)

	require.Equal(t, "Goodbye, World!", string(content))

	closeErr := lf.Close()
	require.NoError(t, closeErr)
}

type lfWriterLine struct {
	sequenceNo int
	writerID   string
}

// Launch two processes that write to the same Lockfile.
// The content of the file is text lines following the pattern:
//
//	<sequence number> <id of the writer>
//
// The test verifies that sequence is always increasing (indicates that file locking is working)
// and that both of the writers are writing to the file.
func TestLockfileConcurrentWrites(t *testing.T) {
	const noOfWriters = 2
	t.Parallel()

	testCtx, cancel := testutil.GetTestContext(t, 15*time.Second)
	defer cancel()

	lockfilePath := filepath.Join(t.TempDir(), t.Name()+".lockfile")
	defer func() {
		_ = os.Remove(lockfilePath) // Best effort cleanup
	}()

	executor := process.NewOSExecutor(log)
	lfWriterToolDir, err := int_testutil.GetTestToolDir("lfwriter")
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(noOfWriters)

	for w := 0; w < noOfWriters; w++ {
		go func(writer int) {
			defer wg.Done()
			cmd := exec.Command("./lfwriter", lockfilePath,
				"--run-duration", "2s",
				"--instance-name", fmt.Sprintf("writer%d", writer),
				"--write-interval", "50ms",
			)
			cmd.Dir = lfWriterToolDir
			stdOutBuf := new(bytes.Buffer)
			stdErrBuf := new(bytes.Buffer)
			cmd.Stdout = stdOutBuf
			cmd.Stderr = stdErrBuf

			exitCode, runErr := process.RunToCompletion(testCtx, executor, cmd)
			require.NoError(t, runErr, "lfwriter process failed. Stdoout: %s, Stderr: %s", stdOutBuf.String(), stdErrBuf.String())
			require.Equal(t, int32(0), exitCode, "lfwriter process returned non-zero exit code. Stdoout: %s, Stderr: %s", stdOutBuf.String(), stdErrBuf.String())
		}(w)
	}

	wg.Wait()

	lines := getLfwriterFileContent(t, lockfilePath)
	require.Greater(t, len(lines), 0, "Lockfile is empty")

	require.Equal(t, noOfWriters, len(maps.SliceToMap(lines, func(l lfWriterLine) (string, bool) {
		return l.writerID, true
	})), "Not all writers wrote to the Lockfile")

	previous := -1
	for _, line := range lines {
		require.Greater(t, line.sequenceNo, previous, "Sequence numbers should be monotonically increasing--file locking is not working correctly")
		previous = line.sequenceNo
	}
}

// Similar to TestLockfileConcurrentWrites, but the writers truncate the file after a certain number of writes.
func TestLockfileConcurrentWritesWithTruncation(t *testing.T) {
	const noOfWriters = 3
	t.Parallel()

	testCtx, cancel := testutil.GetTestContext(t, 15*time.Second)
	defer cancel()

	lockfilePath := filepath.Join(t.TempDir(), t.Name()+".lockfile")
	defer func() {
		_ = os.Remove(lockfilePath) // Best effort cleanup
	}()

	executor := process.NewOSExecutor(log)
	lfWriterToolDir, err := int_testutil.GetTestToolDir("lfwriter")
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(noOfWriters)

	for w := 0; w < noOfWriters; w++ {
		go func(writer int) {
			defer wg.Done()
			cmd := exec.Command("./lfwriter", lockfilePath,
				"--run-duration", "2s",
				"--instance-name", fmt.Sprintf("writer%d", writer),
				"--write-interval", "50ms",
				"--truncate-after", "20",
			)
			cmd.Dir = lfWriterToolDir
			stdOutBuf := new(bytes.Buffer)
			stdErrBuf := new(bytes.Buffer)
			cmd.Stdout = stdOutBuf
			cmd.Stderr = stdErrBuf

			exitCode, runErr := process.RunToCompletion(testCtx, executor, cmd)
			require.NoError(t, runErr, "lfwriter process failed. Stdoout: %s, Stderr: %s", stdOutBuf.String(), stdErrBuf.String())
			require.Equal(t, int32(0), exitCode, "lfwriter process returned non-zero exit code. Stdoout: %s, Stderr: %s", stdOutBuf.String(), stdErrBuf.String())
		}(w)
	}

	wg.Wait()

	lines := getLfwriterFileContent(t, lockfilePath)
	require.Greater(t, len(lines), 0, "Lockfile is empty")

	// Because of truncation and timing not all writers may have left their entries in the file and that is OK.
	// But we do want to check that the sequence numbers are monotonically increasing.

	previous := -1
	for _, line := range lines {
		require.Greater(t, line.sequenceNo, previous, "Sequence numbers should be monotonically increasing--file locking is not working correctly")
		previous = line.sequenceNo
	}
}

func getLfwriterFileContent(t *testing.T, path string) []lfWriterLine {
	lf, err := os.Open(path)
	require.NoError(t, err)
	defer func() {
		_ = lf.Close()
	}()

	var lines []lfWriterLine
	scanner := bufio.NewScanner(lf)
	for scanner.Scan() {
		line := scanner.Text()
		var seqNo int
		var writerID string
		_, sscanfErr := fmt.Sscanf(line, "%d %s", &seqNo, &writerID)
		require.NoError(t, sscanfErr)
		lines = append(lines, lfWriterLine{
			sequenceNo: seqNo,
			writerID:   writerID,
		})
	}

	require.NoError(t, scanner.Err(), "reading lockfile content failed")
	return lines
}
