/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/termpty"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

type terminalCleanupContextKey struct{}

type terminalCleanupTestExecutor struct {
	process.Executor

	stopContextErr error
	stopValue      any
	hasDeadline    bool
	stopCalls      int
}

func (executor *terminalCleanupTestExecutor) StopProcess(
	ctx context.Context,
	_ process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	executor.stopCalls++
	executor.stopContextErr = ctx.Err()
	executor.stopValue = ctx.Value(terminalCleanupContextKey{})
	_, executor.hasDeadline = ctx.Deadline()
	return nil
}

func TestCloseTerminalResourcesDetachesStopFromCanceledContext(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(context.WithValue(
		context.Background(),
		terminalCleanupContextKey{},
		"retained",
	))
	parentCancel()

	testPty := internal_testutil.NewTestPty()
	t.Cleanup(func() { _ = testPty.Close() })
	executor := &terminalCleanupTestExecutor{}
	rcd := &runningContainerData{
		ptp: &termpty.PseudoTerminalProcess{
			PTY:         testPty,
			Handle:      process.NewHandle(4300, time.Unix(1000, 0).UTC()),
			ExitHandler: process.NewConcurrentProcessExitHandler(),
			Executor:    executor,
		},
	}

	rcd.closeTerminalResources(parentCtx, executor, logr.Discard())

	require.Equal(t, 1, executor.stopCalls)
	require.NoError(t, executor.stopContextErr)
	require.Equal(t, "retained", executor.stopValue)
	require.True(t, executor.hasDeadline)
	require.Nil(t, rcd.ptp)
	require.Nil(t, rcd.connMgr)
}
