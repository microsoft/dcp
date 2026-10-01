/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that large error sets render a bounded summary while retaining
// representative errors for errors.Is checks.
func TestSummarizeProcessErrorsBoundsRenderedOutput(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first")
	processErrors := []error{nil, firstErr}
	for index := 1; index < 100; index++ {
		processErrors = append(processErrors, fmt.Errorf("inspection failure %d", index))
	}

	summaryErr := summarizeProcessErrors(processErrors)

	require.ErrorIs(t, summaryErr, firstErr)
	require.Contains(t, summaryErr.Error(), "100 errors occurred")
	require.Contains(t, summaryErr.Error(), "showing first 5")
	require.NotContains(t, summaryErr.Error(), "inspection failure 99")
	require.Less(t, len(summaryErr.Error()), 500)
}

// Verifies that small error sets retain every non-nil error without adding
// truncation diagnostics.
func TestSummarizeProcessErrorsPreservesSmallErrorSets(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first")
	secondErr := errors.New("second")
	summaryErr := summarizeProcessErrors([]error{firstErr, nil, secondErr})

	require.ErrorIs(t, summaryErr, firstErr)
	require.ErrorIs(t, summaryErr, secondErr)
	require.NotContains(t, summaryErr.Error(), "showing first")
}
