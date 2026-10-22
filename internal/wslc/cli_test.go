/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"bytes"
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

// Verifies that WSLC missing-object error codes map to the shared not-found error.
func TestNormalizeCliErrorsRecognizesWslcMissingObjects(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		message string
		match   containers.ErrorMatch
	}{
		{
			name:    "container",
			message: "WSLC_E_CONTAINER_NOT_FOUND\n",
			match:   containerNotFoundMatch,
		},
		{
			name:    "image",
			message: "WSLC_E_IMAGE_NOT_FOUND\n",
			match:   imageNotFoundMatch,
		},
		{
			name:    "network",
			message: "WSLC_E_NETWORK_NOT_FOUND\n",
			match:   networkNotFoundMatch,
		},
		{
			name:    "volume",
			message: "WSLC_E_VOLUME_NOT_FOUND\n",
			match:   volumeNotFoundMatch,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			normalizedErr := normalizeCliErrors(bytes.NewBufferString(testCase.message), testCase.match)
			require.ErrorIs(t, normalizedErr, containers.ErrNotFound)
			require.False(t, errors.Is(normalizedErr, containers.ErrUnmatched))
		})
	}
}

// Verifies that WSLC session and VM error codes mark the runtime unhealthy while registry connection failures do not.
func TestNormalizeCliErrorsRestrictsRuntimeHealthClassification(t *testing.T) {
	t.Parallel()

	registryErr := normalizeCliErrors(bytes.NewBufferString(
		"failed to connect to registry.example.test: connection refused\n",
	))
	require.ErrorIs(t, registryErr, containers.ErrUnmatched)
	require.NotErrorIs(t, registryErr, containers.ErrRuntimeNotHealthy)

	sessionManagerErr := normalizeCliErrors(bytes.NewBufferString(
		"WSLC_E_SESSION_NOT_FOUND\n",
	))
	require.ErrorIs(t, sessionManagerErr, containers.ErrRuntimeNotHealthy)
	require.NotErrorIs(t, sessionManagerErr, containers.ErrUnmatched)

	defaultSessionErr := normalizeCliErrors(bytes.NewBufferString(
		"WSLC_E_VM_NOT_RUNNING\n",
	))
	require.ErrorIs(t, defaultSessionErr, containers.ErrRuntimeNotHealthy)
}

// Verifies that normalizeCliErrors recognizes engine subnet failures and socket conflicts without classifying generic HRESULTs as allocation failures.
func TestNormalizeCliErrorsRecognizesWslcAllocationFailures(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		message string
		wantErr error
	}{
		{
			name:    "subnet exhaustion with localized prefix",
			message: "Erreur du moteur : all predefined address pools have been fully subnetted\r\nCode d'erreur : E_INVALIDARG\r\n",
			wantErr: containers.ErrCouldNotAllocate,
		},
		{
			name:    "allocation failure",
			message: "could not allocate a subnet\r\nError code: E_FAIL\r\n",
			wantErr: containers.ErrCouldNotAllocate,
		},
		{
			name:    "subnet unavailable",
			message: "no available subnet\r\nError code: E_FAIL\r\n",
			wantErr: containers.ErrCouldNotAllocate,
		},
		{name: "symbolic socket conflict", message: "WSAEADDRINUSE\r\n", wantErr: containers.ErrCouldNotAllocate},
		{name: "hexadecimal socket conflict", message: "0x80072740\r\n", wantErr: containers.ErrCouldNotAllocate},
		{name: "unrelated invalid input", message: "Error code: E_INVALIDARG\r\n", wantErr: containers.ErrUnmatched},
		{name: "generic failure", message: "Error code: E_FAIL\r\n", wantErr: containers.ErrUnmatched},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			normalizedErr := normalizeCliErrors(bytes.NewBufferString(testCase.message), allocationFailureMatch)
			require.ErrorIs(t, normalizedErr, testCase.wantErr)
			if testCase.wantErr == containers.ErrUnmatched {
				require.NotErrorIs(t, normalizedErr, containers.ErrCouldNotAllocate)
			}
		})
	}
}

// Verifies that asId trims surrounding whitespace, preserves embedded whitespace, and rejects empty or multiple identifiers.
func TestAsIdRejectsEmptyAndAmbiguousOutput(t *testing.T) {
	t.Parallel()

	identifier, identifierErr := asId(bytes.NewBufferString("\r\n id-value \r\n"))
	require.NoError(t, identifierErr)
	require.Equal(t, "id-value", identifier)

	embeddedWhitespace, whitespaceErr := asId(bytes.NewBufferString("id with spaces\tand tabs\n"))
	require.NoError(t, whitespaceErr)
	require.Equal(t, "id with spaces\tand tabs", embeddedWhitespace)

	_, emptyErr := asId(bytes.NewBuffer(nil))
	require.Error(t, emptyErr)

	_, multipleErr := asId(bytes.NewBufferString("first\nsecond\n"))
	require.Error(t, multipleErr)
}

// Verifies that WSLC inspection classifies completed missing-object responses without treating invalid output or execution failures as absence.
func TestInspectMissingObjectsRequiresExitCodeAndValidResponse(t *testing.T) {
	t.Parallel()

	operations := []struct {
		name    string
		inspect func(context.Context, *WslcCliOrchestrator) (int, error)
	}{
		{"volume", func(ctx context.Context, orchestrator *WslcCliOrchestrator) (int, error) {
			objects, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{"present", "missing"}})
			return len(objects), inspectErr
		}},
		{"image", func(ctx context.Context, orchestrator *WslcCliOrchestrator) (int, error) {
			objects, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{Images: []string{"present", "missing"}})
			return len(objects), inspectErr
		}},
		{"network", func(ctx context.Context, orchestrator *WslcCliOrchestrator) (int, error) {
			objects, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{"present", "missing"}})
			return len(objects), inspectErr
		}},
		{"container", func(ctx context.Context, orchestrator *WslcCliOrchestrator) (int, error) {
			objects, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{"present", "missing"}})
			return len(objects), inspectErr
		}},
	}
	responses := []struct {
		name       string
		stdout     string
		exitCode   int32
		cancel     bool
		startupErr error
		wantCount  int
		wantErr    error
	}{
		{name: "found", stdout: `[{"Id":"id","Name":"present"},{"Id":"other-id","Name":"missing"}]`, wantCount: 2},
		{name: "missing", stdout: "[]\r\n", exitCode: commandFailureExitCode, wantErr: containers.ErrNotFound},
		{name: "partial", stdout: `[{"Id":"id","Name":"present"}]`, exitCode: commandFailureExitCode, wantCount: 1, wantErr: containers.ErrNotFound},
		{name: "empty", exitCode: commandFailureExitCode, wantErr: containers.ErrUnmarshalling},
		{name: "null", stdout: "null", exitCode: commandFailureExitCode, wantErr: containers.ErrUnmarshalling},
		{name: "malformed", stdout: "[", exitCode: commandFailureExitCode, wantErr: containers.ErrUnmarshalling},
		{name: "invalid object", stdout: "[{}]", exitCode: commandFailureExitCode, wantErr: containers.ErrUnmarshalling},
		{name: "null object", stdout: "[null]", exitCode: commandFailureExitCode, wantErr: containers.ErrUnmarshalling},
		{name: "invalid partial", stdout: `[{"Id":"id","Name":"present"},{}]`, exitCode: commandFailureExitCode, wantCount: 1, wantErr: containers.ErrUnmarshalling},
		{name: "wrong exit code", stdout: "[]", exitCode: 2, wantErr: containers.ErrUnmatched},
		{name: "failed with full response", stdout: `[{"Id":"id","Name":"present"},{"Id":"other-id","Name":"missing"}]`, exitCode: commandFailureExitCode, wantCount: 2, wantErr: containers.ErrUnmatched},
		{name: "successful but short", stdout: "[]", wantErr: containers.ErrIncomplete},
		{name: "canceled after output", stdout: "[]", exitCode: commandFailureExitCode, cancel: true, wantErr: context.Canceled},
		{name: "launch failed", startupErr: exec.ErrNotFound, wantErr: exec.ErrNotFound},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			for _, response := range responses {
				t.Run(response.name, func(t *testing.T) {
					t.Parallel()
					lifetimeCtx, lifetimeCancel := testutil.GetTestContext(t, 0)
					defer lifetimeCancel()
					ctx, cancel := context.WithCancel(lifetimeCtx)
					defer cancel()
					executor := internal_testutil.NewTestProcessExecutor(lifetimeCtx)
					autoExecution := internal_testutil.AutoExecution{
						Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"wslc", operation.name, "inspect"}},
						RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
							_, outputErr := io.WriteString(execution.Cmd.Stdout, response.stdout)
							assert.NoError(t, outputErr)
							_, diagnosticErr := io.WriteString(execution.Cmd.Stderr, "Objet introuvable\r\n")
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
					count, inspectErr := operation.inspect(ctx, orchestrator)
					require.Equal(t, response.wantCount, count)
					if response.wantErr == nil {
						require.NoError(t, inspectErr)
					} else {
						require.ErrorIs(t, inspectErr, response.wantErr)
					}
					if response.wantErr == containers.ErrNotFound {
						require.NotErrorIs(t, inspectErr, containers.ErrUnmatched)
						require.ErrorContains(t, inspectErr, "Objet introuvable")
					} else {
						require.NotErrorIs(t, inspectErr, containers.ErrNotFound)
					}
					require.Len(t, executor.Executions, 1)
				})
			}
		})
	}
}
