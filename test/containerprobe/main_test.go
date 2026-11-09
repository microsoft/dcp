/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestRunReport(t *testing.T) {
	t.Setenv("DCP_TEST_VALUE", "value")

	workingDirectory, workingDirectoryErr := os.Getwd()
	require.NoError(t, workingDirectoryErr)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	runErr := run([]string{"report", "DCP_TEST_VALUE", "stderr"}, bytes.NewReader(nil), &stdout, &stderr)

	require.NoError(t, runErr)
	require.Equal(t, "value:"+workingDirectory, stdout.String())
	require.Equal(t, "stderr", stderr.String())
}

func TestRunInspectFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link creation requires additional privileges on Windows")
	}

	tempDirectory := t.TempDir()
	firstPath := filepath.Join(tempDirectory, "first")
	secondPath := filepath.Join(tempDirectory, "second")
	linkPath := filepath.Join(tempDirectory, "link")
	require.NoError(t, usvc_io.WriteFile(firstPath, []byte("first"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, usvc_io.WriteFile(secondPath, []byte("second"), osutil.PermissionOnlyOwnerReadWrite))
	require.NoError(t, os.Symlink("first", linkPath))
	var stdout bytes.Buffer

	runErr := run(
		[]string{"inspect-files", firstPath, secondPath, linkPath},
		bytes.NewReader(nil),
		&stdout,
		&bytes.Buffer{},
	)

	require.NoError(t, runErr)
	require.Equal(t, "first|second|first", stdout.String())
}

func TestRunEmit(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	runErr := run([]string{"emit", "stdout", "stderr"}, bytes.NewReader(nil), &stdout, &stderr)

	require.NoError(t, runErr)
	require.Equal(t, "stdout", stdout.String())
	require.Equal(t, "stderr", stderr.String())
}

func TestRunWriteAndRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "value")
	runWriteErr := run(
		[]string{"write-and-wait", path, "contents", "0s"},
		bytes.NewReader(nil),
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	require.NoError(t, runWriteErr)

	var stdout bytes.Buffer
	runReadErr := run(
		[]string{"wait-read", path, "1s"},
		bytes.NewReader(nil),
		&stdout,
		&bytes.Buffer{},
	)
	require.NoError(t, runReadErr)
	require.Equal(t, "contents", stdout.String())
}

func TestRunInteractive(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	runErr := run(
		[]string{"interactive"},
		bytes.NewBufferString("hello\nexit\nignored\n"),
		&stdout,
		&bytes.Buffer{},
	)

	require.NoError(t, runErr)
	require.Equal(t, "probe:hello\n", stdout.String())
}

func TestRunWaitAndExit(t *testing.T) {
	t.Parallel()

	require.NoError(t, run([]string{"wait", "0s"}, bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
	require.NoError(t, run([]string{"exit"}, bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}))
}

func TestRunRejectsInvalidCommand(t *testing.T) {
	t.Parallel()

	runErr := run([]string{"missing"}, bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{})

	require.EqualError(t, runErr, `unknown command "missing"`)
}
