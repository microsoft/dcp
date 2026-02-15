/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateAnnotationsSize(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		annotations    map[string]string
		expectError    bool
		errorContains  string
	}{
		{
			name:        "nil annotations should pass",
			annotations: nil,
			expectError: false,
		},
		{
			name:        "empty annotations should pass",
			annotations: map[string]string{},
			expectError: false,
		},
		{
			name: "small annotations should pass",
			annotations: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			expectError: false,
		},
		{
			name: "annotations at 90% of limit should pass",
			annotations: map[string]string{
				"large-value": strings.Repeat("a", AnnotationSizeWarningThreshold-11), // 11 = len("large-value")
			},
			expectError: false,
		},
		{
			name: "annotations exactly at limit should pass",
			annotations: map[string]string{
				"large-value": strings.Repeat("a", MaxAnnotationsTotalSize-11), // 11 = len("large-value")
			},
			expectError: false,
		},
		{
			name: "annotations exceeding limit should fail",
			annotations: map[string]string{
				"large-value": strings.Repeat("a", MaxAnnotationsTotalSize), // This exceeds the limit
			},
			expectError:   true,
			errorContains: "Too long",
		},
		{
			name: "multiple annotations exceeding limit should fail",
			annotations: map[string]string{
				"key1": strings.Repeat("a", MaxAnnotationsTotalSize/2),
				"key2": strings.Repeat("b", MaxAnnotationsTotalSize/2+100), // Combined exceeds limit
			},
			expectError:   true,
			errorContains: "Too long",
		},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fieldPath := field.NewPath("metadata", "annotations")
			errors := ValidateAnnotationsSize(tc.annotations, fieldPath)

			if tc.expectError {
				require.NotEmpty(t, errors, "expected validation error but got none")
				require.Contains(t, errors[0].Error(), tc.errorContains,
					"error message should contain expected text")
				require.Equal(t, "metadata.annotations", errors[0].Field,
					"error should reference the correct field path")
			} else {
				require.Empty(t, errors, "expected no validation error but got: %v", errors)
			}
		})
	}
}

func TestCalculateAnnotationsSize(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		annotations  map[string]string
		expectedSize int
	}{
		{
			name:         "nil annotations",
			annotations:  nil,
			expectedSize: 0,
		},
		{
			name:         "empty annotations",
			annotations:  map[string]string{},
			expectedSize: 0,
		},
		{
			name: "single annotation",
			annotations: map[string]string{
				"key": "value",
			},
			expectedSize: 8, // len("key") + len("value") = 3 + 5 = 8
		},
		{
			name: "multiple annotations",
			annotations: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			expectedSize: 20, // (4 + 6) + (4 + 6) = 20
		},
		{
			name: "empty value",
			annotations: map[string]string{
				"key": "",
			},
			expectedSize: 3, // len("key") + len("") = 3 + 0 = 3
		},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			size := calculateAnnotationsSize(tc.annotations)
			require.Equal(t, tc.expectedSize, size)
		})
	}
}

func TestGetAnnotationsSizeInfo(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		"key": "value",
	}

	info := GetAnnotationsSizeInfo(annotations)

	require.Contains(t, info, "8 bytes", "should contain the calculated size")
	require.Contains(t, info, "262144 bytes", "should contain the limit")
	require.Contains(t, info, "256 KB", "should contain the human-readable limit")
}

func TestMaxAnnotationsTotalSizeConstant(t *testing.T) {
	t.Parallel()

	// Verify the constant matches the Kubernetes limit
	require.Equal(t, 262144, MaxAnnotationsTotalSize,
		"MaxAnnotationsTotalSize should be 256 KB (262144 bytes)")
}

func TestAnnotationSizeWarningThresholdConstant(t *testing.T) {
	t.Parallel()

	// Verify the warning threshold is 90% of the max
	expectedThreshold := MaxAnnotationsTotalSize * 90 / 100
	require.Equal(t, expectedThreshold, AnnotationSizeWarningThreshold,
		"AnnotationSizeWarningThreshold should be 90%% of MaxAnnotationsTotalSize")
}
