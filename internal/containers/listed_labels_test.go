/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that ParseListedLabels preserves commas within values when subsequent segments are not labels.
func TestParseListedLabelsPreservesCommaValues(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		raw      string
		expected map[string]string
	}{
		{
			name:     "empty labels",
			expected: map[string]string{},
		},
		{
			name: "comma in value with following label",
			raw:  "workload=one,two,owner=dcp",
			expected: map[string]string{
				"workload": "one,two",
				"owner":    "dcp",
			},
		},
		{
			name: "empty and equals-containing values",
			raw:  "empty=,token=a=b,c,,d",
			expected: map[string]string{
				"empty": "",
				"token": "a=b,c,,d",
			},
		},
		{
			name:     "leading fragment",
			raw:      "fragment,owner=dcp",
			expected: map[string]string{"owner": "dcp"},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, testCase.expected, ParseListedLabels(testCase.raw))
		})
	}
}
