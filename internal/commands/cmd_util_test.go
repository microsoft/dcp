/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that structured exit errors preserve both their exit code and reusable logging disposition through wrapping.
func TestExitErrorMetadataPreservesCodeAndLogMode(t *testing.T) {
	t.Parallel()

	testErr := errors.New("command result")
	for _, testCase := range []struct {
		name         string
		err          error
		fallbackCode int
		expectedCode int
		expectedMode exitErrorLogMode
	}{
		{
			name:         "plain error",
			err:          testErr,
			fallbackCode: 7,
			expectedCode: 7,
			expectedMode: exitErrorLogAsError,
		},
		{
			name:         "error-level exit",
			err:          NewExitCodeError(testErr, 20),
			fallbackCode: 7,
			expectedCode: 20,
			expectedMode: exitErrorLogAsError,
		},
		{
			name:         "informational exit",
			err:          NewInformationalExitCodeError(testErr, 21),
			fallbackCode: 7,
			expectedCode: 21,
			expectedMode: exitErrorLogAsInfo,
		},
		{
			name:         "wrapped silent exit",
			err:          fmt.Errorf("wrapped: %w", NewSilentExitCodeError(testErr, 22)),
			fallbackCode: 7,
			expectedCode: 22,
			expectedMode: exitErrorLogSilent,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			exitCode, logMode := exitErrorMetadata(testCase.err, testCase.fallbackCode)

			require.Equal(t, testCase.expectedCode, exitCode)
			require.Equal(t, testCase.expectedMode, logMode)
		})
	}
}
