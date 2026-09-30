//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/dcppaths"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const wslcCancellationHelperMarker = "DCP_WSLC_CANCELLATION_HELPER_MARKER"

// Verifies that the production dcpproc console-stop path cancels an isolated-console command without the generic six-second delay.
func TestWslcCommandCancellationUsesDcpStopProcessTree(t *testing.T) {
	if markerPath := os.Getenv(wslcCancellationHelperMarker); markerPath != "" {
		require.NoError(t, usvc_io.WriteFile(markerPath, []byte("ready"), 0o600))
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		defer signal.Stop(interrupts)
		select {
		case <-interrupts:
			return
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}

	dcppaths.EnableTestPathProbing()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := process.NewOSExecutor(testr.New(t))
	defer executor.Dispose()
	orchestrator := NewWslcCliOrchestrator(testr.New(t), executor).(*WslcCliOrchestrator)

	markerPath := filepath.Join(t.TempDir(), "ready")
	command := exec.Command(os.Args[0], "-test.run=^TestWslcCommandCancellationUsesDcpStopProcessTree$")
	command.Env = append(os.Environ(), wslcCancellationHelperMarker+"="+markerPath)
	configureWslcCommand(command)

	commandCtx, commandCancel := context.WithCancel(testCtx)
	result := make(chan error, 1)
	go func() {
		_, _, runErr := orchestrator.runBufferedWslcCommand(
			commandCtx,
			"CancellationTest",
			command,
			nil,
			nil,
			time.Minute,
		)
		result <- runErr
	}()

	readyErr := wait.PollUntilContextCancel(testCtx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		_, statErr := os.Stat(markerPath)
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return statErr == nil, statErr
	})
	require.NoError(t, readyErr)

	startedAt := time.Now()
	commandCancel()
	select {
	case runErr := <-result:
		require.ErrorIs(t, runErr, context.Canceled)
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	require.Less(t, time.Since(startedAt), 5500*time.Millisecond)
}
