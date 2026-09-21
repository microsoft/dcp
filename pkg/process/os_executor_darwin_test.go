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

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
)

// Remove this test with the runtime overlay once DCP requires a Go release containing golang/go#81009.
func TestStartProcessChildDoesNotInheritSIGINFOWithDefaultHandler(t *testing.T) {
	t.Parallel()

	signalDispositionTool, toolPathErr := int_testutil.GetTestToolPath("signal-disposition")
	require.NoError(
		t,
		toolPathErr,
		"could not locate signal-disposition test tool (did you run `make test-prereqs`?)",
	)

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	testDeadline, haveTestDeadline := testCtx.Deadline()
	require.True(t, haveTestDeadline, "signal disposition test context should have a deadline")

	launcherTimeout := time.Until(testDeadline)
	require.Positive(t, launcherTimeout, "signal disposition test deadline should be in the future")

	launcherCmd := exec.CommandContext(
		testCtx,
		signalDispositionTool,
		"--timeout", launcherTimeout.String(),
	)
	var launcherOutput bytes.Buffer
	launcherCmd.Stdout = &launcherOutput
	launcherCmd.Stderr = &launcherOutput

	runErr := launcherCmd.Run()
	require.NoError(t, runErr, "signal disposition launcher failed:\n%s", launcherOutput.String())
}
