/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/stretchr/testify/require"
)

func TestUnifyLogsOrdersByName(t *testing.T) {
	t.Parallel()

	dcpRootDir, findRootErr := osutil.FindRootFor(osutil.DirTarget, "test")
	require.NoError(t, findRootErr, "Failed to find root directory for ./test")

	logFileNames := []string{
		filepath.Join(dcpRootDir, "test", "data", "11111-alog.log"),
		filepath.Join(dcpRootDir, "test", "data", "11111-blog.log"),
		filepath.Join(dcpRootDir, "test", "data", "11111-clog.log"),
	}

	logFiles := make([]*os.File, 0, len(logFileNames))
	for _, logFileName := range logFileNames {
		logFile, openErr := os.Open(logFileName)
		require.NoError(t, openErr)
		defer logFile.Close()
		logFiles = append(logFiles, logFile)
	}

	destination := bytes.NewBuffer([]byte{})
	unifyErr := unifyLogs(destination, logFiles...)
	require.NoError(t, unifyErr)

	expectedLines := []string{
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.338-0700\",\"logger\":\"alog\",\"msg\":\"First\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.339-0700\",\"logger\":\"alog\",\"msg\":\"Second\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.339-0700\",\"logger\":\"alog\",\"msg\":\"Third\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.339-0700\",\"logger\":\"blog\",\"msg\":\"Fourth\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.339-0700\",\"logger\":\"clog\",\"msg\":\"Fifth\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.340-0700\",\"logger\":\"blog\",\"msg\":\"Sixth\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.340-0700\",\"logger\":\"blog\",\"msg\":\"Seventh\"}",
		"{\"level\":\"debug\",\"ts\":\"2025-09-04T10:10:50.340-0700\",\"logger\":\"clog\",\"msg\":\"Eighth\"}",
	}

	actualLines := make([]string, 0, len(expectedLines))

	scanner := bufio.NewScanner(destination)
	for scanner.Scan() {
		line := scanner.Text()
		actualLines = append(actualLines, line)
	}

	require.Equal(t, expectedLines, actualLines)
}

func TestCreateSessionLogOutputFileTruncatesExistingFile(t *testing.T) {
	t.Parallel()

	workingDirectory, workingDirectoryErr := os.Getwd()
	require.NoError(t, workingDirectoryErr)
	tempDirectory, tempDirectoryErr := os.MkdirTemp(workingDirectory, "session-log-output-test-*")
	require.NoError(t, tempDirectoryErr)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tempDirectory))
	})
	absolutePath := filepath.Join(tempDirectory, "session.log")
	path, relativePathErr := filepath.Rel(workingDirectory, absolutePath)
	require.NoError(t, relativePathErr)
	initialFile, initialCreateErr := createSessionLogOutputFile(path)
	require.NoError(t, initialCreateErr)
	_, initialWriteErr := initialFile.WriteString("existing")
	require.NoError(t, initialWriteErr)
	require.NoError(t, initialFile.Close())

	replacementFile, replacementCreateErr := createSessionLogOutputFile(path)
	require.NoError(t, replacementCreateErr)
	_, replacementWriteErr := replacementFile.WriteString("new")
	require.NoError(t, replacementWriteErr)
	require.NoError(t, replacementFile.Close())

	contents, readErr := os.ReadFile(absolutePath)
	require.NoError(t, readErr)
	require.Equal(t, "new", string(contents))
}
