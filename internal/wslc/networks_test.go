/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that RemoveNetworks uses the resolved name, tolerates disappearance after resolution, and preserves execution errors.
func TestRemoveNetworksForcesDeletionAfterResolution(t *testing.T) {
	t.Parallel()

	responses := []struct {
		name       string
		stdout     string
		stderr     string
		exitCode   int32
		cancel     bool
		startupErr error
		wantErr    error
	}{
		{name: "removed", stdout: "network\r\n"},
		{name: "disappeared after resolution"},
		{name: "active endpoints", stderr: "Rede indisponivel\r\nERROR_SHARING_VIOLATION\r\n", exitCode: 1, wantErr: containers.ErrObjectInUse},
		{name: "unclassified failure", stderr: "Falha\r\nE_FAIL\r\n", exitCode: 1, wantErr: containers.ErrUnmatched},
		{name: "failed after output", stdout: "network\r\n", exitCode: 1, wantErr: errCommandFailed},
		{name: "canceled", cancel: true, wantErr: context.Canceled},
		{name: "launch failed", startupErr: exec.ErrNotFound, wantErr: exec.ErrNotFound},
	}
	for _, force := range []bool{false, true} {
		for _, response := range responses {
			t.Run(response.name+map[bool]string{false: "/ordinary", true: "/force"}[force], func(t *testing.T) {
				t.Parallel()
				lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, 0)
				defer lifetimeCancel()
				ctx, cancel := context.WithCancel(lifetimeCtx)
				defer cancel()
				executor := internal_testutil.NewTestProcessExecutor(lifetimeCtx)
				executor.InstallAutoExecution(internal_testutil.AutoExecution{
					Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"wslc", "network", "inspect"}},
					RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
						assert.Equal(t, []string{"network", "inspect", "--format", "json", "network-id"}, execution.Cmd.Args[1:])
						_, inspectOutputErr := io.WriteString(execution.Cmd.Stdout, `[{"Id":"network-id","Name":"network"}]`)
						assert.NoError(t, inspectOutputErr)
						return 0
					},
				})
				autoExecution := internal_testutil.AutoExecution{
					Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"wslc", "network", "remove"}},
					RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
						assert.Equal(t, []string{"network", "remove", "--force", "network"}, execution.Cmd.Args[1:])
						_, outputErr := io.WriteString(execution.Cmd.Stdout, response.stdout)
						assert.NoError(t, outputErr)
						_, diagnosticErr := io.WriteString(execution.Cmd.Stderr, response.stderr)
						assert.NoError(t, diagnosticErr)
						if response.cancel {
							cancel()
						}
						return response.exitCode
					},
				}
				if response.startupErr != nil {
					autoExecution.StartupError = func(_ *internal_testutil.ProcessExecution) error {
						return response.startupErr
					}
				}
				executor.InstallAutoExecution(autoExecution)
				orchestrator := &WslcCliOrchestrator{log: logr.Discard(), executor: executor}
				removed, removeErr := orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
					Networks: []string{"network-id"},
					Force:    force,
				})
				if response.wantErr != nil {
					require.ErrorIs(t, removeErr, response.wantErr)
					require.Empty(t, removed)
					require.ErrorIs(t, removeErr, containers.ErrIncomplete)
				} else {
					require.NoError(t, removeErr)
					require.Equal(t, []string{"network-id"}, removed)
				}
				require.NotErrorIs(t, removeErr, containers.ErrNotFound)
				require.Len(t, executor.Executions, 2)
			})
		}
	}
}
