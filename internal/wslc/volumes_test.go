/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"errors"
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

// Verifies that RemoveVolumes distinguishes deletion from absence without localized diagnostics and preserves execution and output errors.
func TestRemoveVolumesRecognizesSuccessfulOutput(t *testing.T) {
	t.Parallel()

	responses := []struct {
		name       string
		stdout     string
		stderr     string
		exitCode   int32
		cancel     bool
		startupErr error
		wantErr    error
		wantText   string
		missing    bool
	}{
		{name: "removed", stdout: "volume\r\n"},
		{name: "trimmed name", stdout: "\r\n volume \r\n\r\n"},
		{name: "missing", missing: true},
		{name: "whitespace only", stdout: " \r\n\t", missing: true},
		{name: "wrong name", stdout: "other\r\n", wantText: `returned name "other" instead of "volume"`},
		{name: "multiple names", stdout: "volume\r\nother\r\n", wantText: "does not contain a single identifier"},
		{name: "in use", stderr: "Volume indisponivel\r\nERROR_SHARING_VIOLATION\r\n", exitCode: 1, wantErr: containers.ErrObjectInUse},
		{name: "unclassified failure", stderr: "Falha\r\nE_FAIL\r\n", exitCode: 1, wantErr: containers.ErrUnmatched},
		{name: "failed after output", stdout: "volume\r\n", exitCode: 1, wantErr: errCommandFailed},
		{name: "canceled with empty output", cancel: true, wantErr: context.Canceled},
		{name: "canceled after output", stdout: "volume\r\n", cancel: true, wantErr: context.Canceled},
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
				autoExecution := internal_testutil.AutoExecution{
					Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"wslc", "volume", "remove"}},
					RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
						assert.Equal(t, []string{"volume", "remove", "--force", "volume"}, execution.Cmd.Args[1:])
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
				removed, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
					Volumes: []string{"volume"},
					Force:   force,
				})
				wantErr := response.wantErr
				if response.missing && !force {
					wantErr = containers.ErrNotFound
				}
				if wantErr != nil {
					require.ErrorIs(t, removeErr, wantErr)
				} else if response.wantText != "" {
					require.ErrorContains(t, removeErr, response.wantText)
				} else {
					require.NoError(t, removeErr)
					require.Equal(t, []string{"volume"}, removed)
				}
				if removeErr != nil {
					require.Empty(t, removed)
					require.ErrorIs(t, removeErr, containers.ErrIncomplete)
				}
				if wantErr != containers.ErrNotFound {
					require.NotErrorIs(t, removeErr, containers.ErrNotFound)
				}
				require.Len(t, executor.Executions, 1)
			})
		}
	}
}

// Verifies that RemoveVolumes preserves successful results and per-volume errors when a batch includes missing and in-use volumes.
func TestRemoveVolumesPreservesPartialResults(t *testing.T) {
	t.Parallel()

	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "force"}[force], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			executor := internal_testutil.NewTestProcessExecutor(ctx)
			executor.InstallAutoExecution(internal_testutil.AutoExecution{
				Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"wslc", "volume", "remove"}},
				RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
					assert.Equal(t, []string{"volume", "remove", "--force"}, execution.Cmd.Args[1:4])
					switch execution.Cmd.Args[4] {
					case "present":
						_, outputErr := io.WriteString(execution.Cmd.Stdout, "present\r\n")
						assert.NoError(t, outputErr)
					case "inuse":
						_, diagnosticErr := io.WriteString(execution.Cmd.Stderr, "ERROR_SHARING_VIOLATION\r\n")
						assert.NoError(t, diagnosticErr)
						return 1
					case "missing":
					default:
						t.Errorf("unexpected volume %q", execution.Cmd.Args[4])
						return 1
					}
					return 0
				},
			})
			orchestrator := &WslcCliOrchestrator{log: logr.Discard(), executor: executor}
			removed, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
				Volumes: []string{"present", "missing", "inuse"},
				Force:   force,
			})
			expected := []string{"present"}
			if force {
				expected = append(expected, "missing")
			}
			require.Equal(t, expected, removed)
			require.ErrorIs(t, removeErr, containers.ErrObjectInUse)
			require.ErrorIs(t, removeErr, containers.ErrIncomplete)
			require.Equal(t, !force, errors.Is(removeErr, containers.ErrNotFound))
			require.Len(t, executor.Executions, 3)
		})
	}
}
