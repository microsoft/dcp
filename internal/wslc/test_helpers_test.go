/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	pkg_testutil "github.com/microsoft/dcp/pkg/testutil"
)

func newTestOrchestrator(
	t *testing.T,
) (context.Context, *WslcCliOrchestrator, *internal_testutil.TestProcessExecutor) {
	t.Helper()

	ctx, cancel := pkg_testutil.GetTestContext(t, 20*time.Second)
	t.Cleanup(cancel)
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() {
		require.NoError(t, executor.Close())
	})
	orchestrator := NewWslcCliOrchestrator(testr.New(t), executor).(*WslcCliOrchestrator)
	return ctx, orchestrator, executor
}

func installAutoCommand(
	t *testing.T,
	executor *internal_testutil.TestProcessExecutor,
	command []string,
	stdout string,
	stderr string,
	exitCode int32,
) {
	t.Helper()

	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: command},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			if stdout != "" {
				_, stdoutErr := io.WriteString(execution.Cmd.Stdout, stdout)
				require.NoError(t, stdoutErr)
			}
			if stderr != "" {
				_, stderrErr := io.WriteString(execution.Cmd.Stderr, stderr)
				require.NoError(t, stderrErr)
			}
			return exitCode
		},
	})
}

type testWriteSyncCloser struct {
	lock      sync.Mutex
	buffer    bytes.Buffer
	closed    chan struct{}
	closeOnce sync.Once
	syncCount atomic.Int32
}

func newTestWriteSyncCloser() *testWriteSyncCloser {
	return &testWriteSyncCloser{closed: make(chan struct{})}
}

func (writer *testWriteSyncCloser) Write(data []byte) (int, error) {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	return writer.buffer.Write(data)
}

func (writer *testWriteSyncCloser) Sync() error {
	writer.syncCount.Add(1)
	return nil
}

func (writer *testWriteSyncCloser) Close() error {
	writer.closeOnce.Do(func() {
		close(writer.closed)
	})
	return nil
}

func (writer *testWriteSyncCloser) String() string {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	return writer.buffer.String()
}
