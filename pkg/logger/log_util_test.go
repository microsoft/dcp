/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logger

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that SummarizeErrors limits rendered examples while preserving every
// cause for errors.Is and errors.As, including errors outside the rendered subset.
func TestSummarizeErrorsBoundsRenderedOutput(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first")
	lastErr := &os.PathError{Op: "open", Path: "last", Err: os.ErrPermission}
	summaryErr := SummarizeErrors([]error{
		nil, firstErr, errors.New("second"), errors.New("third"),
		errors.New("fourth"), errors.New("fifth"), lastErr,
	})

	require.ErrorIs(t, summaryErr, firstErr)
	require.ErrorIs(t, summaryErr, lastErr)
	var pathErr *os.PathError
	require.ErrorAs(t, summaryErr, &pathErr)
	require.Same(t, lastErr, pathErr)
	require.Equal(t, "6 errors occurred (showing first 5): first\nsecond\nthird\nfourth\nfifth", summaryErr.Error())
}

// Verifies that SummarizeErrors renders and preserves all causes just below and exactly
// at the five-example cutoff, excluding nil entries from the count.
func TestSummarizeErrorsPreservesErrorsAtRenderingBoundary(t *testing.T) {
	t.Parallel()

	firstErr := errors.New("first")
	errs := []error{firstErr, nil, errors.New("second"), errors.New("third"), errors.New("fourth")}
	belowCutoff := SummarizeErrors(errs)
	require.Equal(t, "first\nsecond\nthird\nfourth", belowCutoff.Error())
	for _, cause := range errs {
		if cause != nil {
			require.ErrorIs(t, belowCutoff, cause)
		}
	}

	fifthErr := errors.New("fifth")
	atCutoff := SummarizeErrors(append(errs, fifthErr))
	require.Equal(t, "first\nsecond\nthird\nfourth\nfifth", atCutoff.Error())
	for _, cause := range errs {
		if cause != nil {
			require.ErrorIs(t, atCutoff, cause)
		}
	}
	require.ErrorIs(t, atCutoff, fifthErr)
}

// Verifies that SummarizeErrors returns nil when there are no non-nil errors.
func TestSummarizeErrorsHandlesEmptySets(t *testing.T) {
	t.Parallel()

	require.NoError(t, SummarizeErrors(nil))
	require.NoError(t, SummarizeErrors([]error{nil, nil}))
}
