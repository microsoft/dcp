/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutableShouldStart(t *testing.T) {
	t.Parallel()

	shouldStart := true
	shouldNotStart := false

	testCases := []struct {
		name     string
		start    *bool
		expected bool
	}{
		{
			name:     "omitted",
			start:    nil,
			expected: true,
		},
		{
			name:     "explicit true",
			start:    &shouldStart,
			expected: true,
		},
		{
			name:     "explicit false",
			start:    &shouldNotStart,
			expected: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			exe := Executable{
				Spec: ExecutableSpec{
					Start: testCase.start,
				},
			}

			assert.Equal(t, testCase.expected, exe.ShouldStart())
		})
	}
}

func TestExecutableSpecEqualTreatsOmittedStartAsExplicitTrue(t *testing.T) {
	t.Parallel()

	shouldStart := true
	shouldNotStart := false

	omittedStart := ExecutableSpec{
		ExecutablePath: "/path/to/app",
	}
	explicitStart := ExecutableSpec{
		ExecutablePath: "/path/to/app",
		Start:          &shouldStart,
	}
	explicitDoNotStart := ExecutableSpec{
		ExecutablePath: "/path/to/app",
		Start:          &shouldNotStart,
	}

	assert.True(t, omittedStart.Equal(explicitStart))
	assert.False(t, omittedStart.Equal(explicitDoNotStart))
	assert.False(t, explicitStart.Equal(explicitDoNotStart))
}

func TestExecutableValidateUpdateStartTransitions(t *testing.T) {
	t.Parallel()

	shouldStart := true
	shouldNotStart := false

	testCases := []struct {
		name        string
		oldStart    *bool
		newStart    *bool
		expectError bool
	}{
		{
			name:     "omitted to explicit true",
			oldStart: nil,
			newStart: &shouldStart,
		},
		{
			name:        "omitted to explicit false",
			oldStart:    nil,
			newStart:    &shouldNotStart,
			expectError: true,
		},
		{
			name:        "explicit true to explicit false",
			oldStart:    &shouldStart,
			newStart:    &shouldNotStart,
			expectError: true,
		},
		{
			name:     "explicit false to explicit true",
			oldStart: &shouldNotStart,
			newStart: &shouldStart,
		},
		{
			name:     "explicit false to omitted",
			oldStart: &shouldNotStart,
			newStart: nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			oldExe := &Executable{
				Spec: ExecutableSpec{
					ExecutablePath: "/path/to/app",
					Start:          testCase.oldStart,
				},
			}
			newExe := &Executable{
				Spec: ExecutableSpec{
					ExecutablePath: "/path/to/app",
					Start:          testCase.newStart,
				},
			}

			errs := newExe.ValidateUpdate(context.Background(), oldExe)
			if testCase.expectError {
				require.NotEmpty(t, errs)
				assert.Equal(t, "spec.start", errs[0].Field)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}
