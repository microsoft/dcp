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

func TestExecutableGetLeaseKey(t *testing.T) {
	t.Parallel()

	exe := &Executable{}
	exe.Namespace = "default"
	exe.Name = "api"

	require.Equal(t, "default/api", exe.GetLeaseKey())
}

func TestExecutableSpecGetLifecycleKeyPreservesStringBoundaries(t *testing.T) {
	t.Parallel()

	spec := ExecutableSpec{
		ExecutablePath:   "A",
		WorkingDirectory: "BC",
		Args:             []string{"A", "BC"},
	}

	key, computed, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.True(t, computed)

	specWithAmbiguousConcatenation := ExecutableSpec{
		ExecutablePath:   "AB",
		WorkingDirectory: "C",
		Args:             []string{"AB", "C"},
	}

	keyWithAmbiguousConcatenation, _, ambiguousConcatenationErr := specWithAmbiguousConcatenation.GetLifecycleKey()
	require.NoError(t, ambiguousConcatenationErr)
	require.NotEqual(t, key, keyWithAmbiguousConcatenation)
}

func TestExecutableGetLifecycleKeyIgnoresImplicitEffectiveEnv(t *testing.T) {
	t.Parallel()

	exe := lifecycleKeyTestExecutable()
	exe.Status.EffectiveArgs = []string{"--port", "5000"}
	exe.Status.EffectiveEnv = []EnvVar{
		{Name: "PATH", Value: "/first/path"},
		{Name: "EXPLICIT", Value: "stable"},
		{Name: "ASPNETCORE_URLS", Value: "http://127.0.0.1:5000"},
	}

	key, computed, keyErr := exe.GetLifecycleKey()
	require.NoError(t, keyErr)
	require.True(t, computed)

	exeWithDifferentImplicitEnv := lifecycleKeyTestExecutable()
	exeWithDifferentImplicitEnv.Status.EffectiveArgs = []string{"--port", "5000"}
	exeWithDifferentImplicitEnv.Status.EffectiveEnv = []EnvVar{
		{Name: "PATH", Value: "/different/path"},
		{Name: "EXPLICIT", Value: "stable"},
		{Name: "ASPNETCORE_URLS", Value: "http://127.0.0.1:5001"},
	}

	keyWithDifferentImplicitEnv, _, differentImplicitEnvErr := exeWithDifferentImplicitEnv.GetLifecycleKey()
	require.NoError(t, differentImplicitEnvErr)
	require.Equal(t, key, keyWithDifferentImplicitEnv)
}

func TestExecutableGetLifecycleKeyIncludesExplicitEffectiveEnv(t *testing.T) {
	t.Parallel()

	exe := lifecycleKeyTestExecutable()
	exe.Status.EffectiveArgs = []string{"--port", "5000"}
	exe.Status.EffectiveEnv = []EnvVar{
		{Name: "EXPLICIT", Value: "first"},
	}

	key, _, keyErr := exe.GetLifecycleKey()
	require.NoError(t, keyErr)

	exeWithDifferentExplicitEnv := lifecycleKeyTestExecutable()
	exeWithDifferentExplicitEnv.Status.EffectiveArgs = []string{"--port", "5000"}
	exeWithDifferentExplicitEnv.Status.EffectiveEnv = []EnvVar{
		{Name: "EXPLICIT", Value: "second"},
	}

	keyWithDifferentExplicitEnv, _, differentExplicitEnvErr := exeWithDifferentExplicitEnv.GetLifecycleKey()
	require.NoError(t, differentExplicitEnvErr)
	require.NotEqual(t, key, keyWithDifferentExplicitEnv)
}

func TestExecutableGetLifecycleKeyIncludesEffectiveArgs(t *testing.T) {
	t.Parallel()

	exe := lifecycleKeyTestExecutable()
	exe.Status.EffectiveArgs = []string{"--port", "5000"}
	exe.Status.EffectiveEnv = []EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	key, _, keyErr := exe.GetLifecycleKey()
	require.NoError(t, keyErr)

	exeWithDifferentEffectiveArgs := lifecycleKeyTestExecutable()
	exeWithDifferentEffectiveArgs.Status.EffectiveArgs = []string{"--port", "5001"}
	exeWithDifferentEffectiveArgs.Status.EffectiveEnv = []EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	keyWithDifferentEffectiveArgs, _, differentEffectiveArgsErr := exeWithDifferentEffectiveArgs.GetLifecycleKey()
	require.NoError(t, differentEffectiveArgsErr)
	require.NotEqual(t, key, keyWithDifferentEffectiveArgs)
}

func TestExecutableGetLifecycleKeyRequiresEffectiveArgs(t *testing.T) {
	t.Parallel()

	exe := lifecycleKeyTestExecutable()
	exe.Status.EffectiveEnv = []EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	_, _, keyErr := exe.GetLifecycleKey()

	require.ErrorContains(t, keyErr, "effective arguments")
}

func TestExecutableGetLifecycleKeyRequiresEffectiveEnv(t *testing.T) {
	t.Parallel()

	exe := lifecycleKeyTestExecutable()
	exe.Status.EffectiveArgs = []string{"--port", "5000"}

	_, _, keyErr := exe.GetLifecycleKey()

	require.ErrorContains(t, keyErr, "effective environment")
}

func lifecycleKeyTestExecutable() *Executable {
	return &Executable{
		Spec: ExecutableSpec{
			ExecutablePath:   "/path/to/app",
			WorkingDirectory: "/path/to/workdir",
			Args:             []string{"--port", "{{ port }}"},
			Env:              []EnvVar{{Name: "EXPLICIT", Value: "{{ value }}"}},
			Persistent:       true,
		},
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
