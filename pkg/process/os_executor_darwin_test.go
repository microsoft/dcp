//go:build darwin

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"bytes"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestStartProcessChildDoesNotInheritSIGINFOWithDefaultHandler(t *testing.T) {
	t.Parallel()

	signalDispositionTool, toolPathErr := int_testutil.GetTestToolPath("signal-disposition")
	require.NoError(
		t,
		toolPathErr,
		"could not locate signal-disposition test tool (did you run `make test-prereqs`?)",
	)

	childCmd := exec.Command(signalDispositionTool)
	var childOutput bytes.Buffer
	childCmd.Stdout = &childOutput
	childCmd.Stderr = &childOutput

	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	exitCode, runErr := process.RunToCompletion(testCtx, executor, childCmd)
	require.NoError(t, runErr, "child process inherited an invalid signal disposition:\n%s", childOutput.String())
	require.Zero(t, exitCode, "child process reported an invalid signal disposition:\n%s", childOutput.String())
}
