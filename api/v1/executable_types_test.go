/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/osutil"
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

// Verifies that LogicalProcessSpec and Executable lifecycle keys agree on effective explicit
// environment equivalence, including overwritten values and Windows case-insensitive duplicate names.
func TestLogicalProcessLifecycleEnvironmentMatchesExecutable(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		original   []EnvVar
		changed    []EnvVar
		equivalent bool
	}{
		{"ordering", []EnvVar{{Name: "B", Value: "2"}, {Name: "A", Value: "1"}},
			[]EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2"}}, true},
		{"overwritten value", []EnvVar{{Name: "A", Value: "first"}, {Name: "A", Value: "last"}},
			[]EnvVar{{Name: "A", Value: "different"}, {Name: "A", Value: "last"}}, true},
		{"redundant declaration", []EnvVar{{Name: "A", Value: "first"}, {Name: "A", Value: "last"}},
			[]EnvVar{{Name: "A", Value: "last"}}, true},
		{"duplicate precedence", []EnvVar{{Name: "A", Value: "first"}, {Name: "A", Value: "last"}},
			[]EnvVar{{Name: "A", Value: "last"}, {Name: "A", Value: "first"}}, false},
		{"case-insensitive duplicate", []EnvVar{{Name: "PATH", Value: "first"}, {Name: "Path", Value: "last"}},
			[]EnvVar{{Name: "Path", Value: "last"}}, osutil.IsWindows()},
		{"case-sensitive declared name", []EnvVar{{Name: "Path", Value: "value"}},
			[]EnvVar{{Name: "PATH", Value: "value"}}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var executableKeys, processKeys [2]string
			for index, environment := range [][]EnvVar{testCase.original, testCase.changed} {
				effective := lifecycleEnvMap()
				for _, variable := range environment {
					effective.Override(variable.Name, variable.Value)
				}
				executable := &Executable{Spec: ExecutableSpec{ExecutablePath: "program", Env: environment}}
				executable.Status.EffectiveEnv = maps.MapToSlice[EnvVar](effective.Data(), func(name, value string) EnvVar {
					return EnvVar{Name: name, Value: value}
				})
				executableKey, _, executableErr := executable.GetLifecycleKey()
				require.NoError(t, executableErr)
				executableKeys[index] = executableKey
				spec := apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "program", Env: environment}}
				original := spec.DeepCopy()
				processKey, _, processErr := spec.GetLifecycleKey()
				require.NoError(t, processErr)
				require.Equal(t, original, &spec)
				processKeys[index] = processKey
			}
			require.Equal(t, testCase.equivalent, executableKeys[0] == executableKeys[1], "V1 environment equivalence")
			require.Equal(t, executableKeys[0] == executableKeys[1], processKeys[0] == processKeys[1], "V2 must match V1 environment equivalence")
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

func TestExecutableSpecGetLifecycleKeyIncludesMonitorFields(t *testing.T) {
	t.Parallel()

	monitorPID := int64(12345)
	monitorTimestamp := metav1.NewMicroTime(time.Now().UTC())
	spec := ExecutableSpec{
		ExecutablePath:   "/path/to/app",
		WorkingDirectory: "/path/to/workdir",
		Persistent:       true,
	}

	key, _, keyErr := spec.GetLifecycleKey()
	require.NoError(t, keyErr)

	spec.MonitorPID = &monitorPID
	spec.MonitorTimestamp = monitorTimestamp
	keyWithMonitor, _, monitorKeyErr := spec.GetLifecycleKey()
	require.NoError(t, monitorKeyErr)
	require.NotEqual(t, key, keyWithMonitor)

	differentMonitorPID := monitorPID + 1
	spec.MonitorPID = &differentMonitorPID
	keyWithDifferentMonitorPID, _, differentMonitorPIDErr := spec.GetLifecycleKey()
	require.NoError(t, differentMonitorPIDErr)
	require.NotEqual(t, keyWithMonitor, keyWithDifferentMonitorPID)

	spec.MonitorPID = &monitorPID
	spec.MonitorTimestamp = metav1.NewMicroTime(monitorTimestamp.Time.Add(time.Second))
	keyWithDifferentMonitorTimestamp, _, differentMonitorTimestampErr := spec.GetLifecycleKey()
	require.NoError(t, differentMonitorTimestampErr)
	require.NotEqual(t, keyWithMonitor, keyWithDifferentMonitorTimestamp)
}

func TestExecutableValidateMonitorFields(t *testing.T) {
	t.Parallel()

	monitorPID := int64(12345)
	negativeMonitorPID := int64(-1)
	monitorTimestamp := metav1.NewMicroTime(time.Now().UTC())

	testCases := []struct {
		name        string
		spec        ExecutableSpec
		expectError bool
	}{
		{
			name: "valid persistent monitor",
			spec: ExecutableSpec{
				ExecutablePath:   "/path/to/app",
				Persistent:       true,
				MonitorPID:       &monitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
		},
		{
			name: "monitor requires persistent",
			spec: ExecutableSpec{
				ExecutablePath:   "/path/to/app",
				MonitorPID:       &monitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
			expectError: true,
		},
		{
			name: "monitor pid requires timestamp",
			spec: ExecutableSpec{
				ExecutablePath: "/path/to/app",
				Persistent:     true,
				MonitorPID:     &monitorPID,
			},
			expectError: true,
		},
		{
			name: "monitor timestamp requires pid",
			spec: ExecutableSpec{
				ExecutablePath:   "/path/to/app",
				Persistent:       true,
				MonitorTimestamp: monitorTimestamp,
			},
			expectError: true,
		},
		{
			name: "monitor pid must be positive",
			spec: ExecutableSpec{
				ExecutablePath:   "/path/to/app",
				Persistent:       true,
				MonitorPID:       &negativeMonitorPID,
				MonitorTimestamp: monitorTimestamp,
			},
			expectError: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			exe := &Executable{Spec: testCase.spec}
			errors := exe.Validate(context.Background())

			if testCase.expectError {
				require.NotEmpty(t, errors)
			} else {
				require.Empty(t, errors)
			}
		})
	}
}

func TestExecutableValidateTerminalPersistentCombination(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		spec        ExecutableSpec
		expectError bool
	}{
		{
			name: "non-persistent with terminal is allowed",
			spec: ExecutableSpec{
				ExecutablePath: "/path/to/app",
				Terminal: &TerminalSpec{
					UDSPath: "/tmp/app.sock",
					Cols:    80,
					Rows:    24,
				},
			},
		},
		{
			name: "persistent without terminal is allowed",
			spec: ExecutableSpec{
				ExecutablePath: "/path/to/app",
				Persistent:     true,
			},
		},
		{
			name: "persistent with terminal is rejected",
			spec: ExecutableSpec{
				ExecutablePath: "/path/to/app",
				Persistent:     true,
				Terminal: &TerminalSpec{
					UDSPath: "/tmp/app.sock",
					Cols:    80,
					Rows:    24,
				},
			},
			expectError: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			exe := &Executable{Spec: testCase.spec}
			errors := exe.Validate(context.Background())

			if testCase.expectError {
				require.NotEmpty(t, errors)
			} else {
				require.Empty(t, errors)
			}
		})
	}
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

func TestExecutableGetLifecycleKeyReturnsEnvFileReadError(t *testing.T) {
	t.Parallel()

	missingEnvFile := filepath.Join(t.TempDir(), "missing.env")
	exe := lifecycleKeyTestExecutable()
	exe.Spec.EnvFiles = []string{missingEnvFile}
	exe.Status.EffectiveArgs = []string{"--port", "5000"}
	exe.Status.EffectiveEnv = []EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	_, _, keyErr := exe.GetLifecycleKey()

	require.ErrorContains(t, keyErr, "could not read executable environment files for lifecycle key")
	require.ErrorContains(t, keyErr, missingEnvFile)
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
