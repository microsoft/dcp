/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"errors"
	"testing"

	"github.com/microsoft/dcp/internal/dcpproc/protocol"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/stretchr/testify/require"
)

type exitCodeSource interface {
	ExitCode() int
}

func TestStopProcessTreeCommandErrorUsesStructuredExitCodes(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		err      error
		exitCode int
	}{
		{
			name:     "incomplete tree",
			err:      process.ErrIncompleteProcessTree,
			exitCode: protocol.StopProcessTreeIncompleteExitCode,
		},
		{
			name:     "process gone",
			err:      &process.ErrProcessNotFound{Pid: 42},
			exitCode: protocol.StopProcessTreeProcessGoneExitCode,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			commandErr := stopProcessTreeCommandError(testCase.err)
			var exitErr exitCodeSource
			require.True(t, errors.As(commandErr, &exitErr))
			require.Equal(t, testCase.exitCode, exitErr.ExitCode())
			require.ErrorIs(t, commandErr, testCase.err)
		})
	}
}
