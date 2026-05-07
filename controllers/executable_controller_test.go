/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

func TestPersistentExecutableLifecycleKeyIgnoresImplicitEffectiveEnv(t *testing.T) {
	t.Parallel()

	exe := persistentLifecycleKeyTestExecutable()
	exe.Status.EffectiveEnv = []apiv1.EnvVar{
		{Name: "PATH", Value: "/first/path"},
		{Name: "EXPLICIT", Value: "stable"},
		{Name: "ASPNETCORE_URLS", Value: "http://127.0.0.1:5000"},
	}

	key, computed, keyErr := persistentExecutableLifecycleKey(exe)
	require.NoError(t, keyErr)
	require.True(t, computed)

	exeWithDifferentImplicitEnv := persistentLifecycleKeyTestExecutable()
	exeWithDifferentImplicitEnv.Status.EffectiveEnv = []apiv1.EnvVar{
		{Name: "PATH", Value: "/different/path"},
		{Name: "EXPLICIT", Value: "stable"},
		{Name: "ASPNETCORE_URLS", Value: "http://127.0.0.1:5001"},
	}

	keyWithDifferentImplicitEnv, _, differentImplicitEnvErr := persistentExecutableLifecycleKey(exeWithDifferentImplicitEnv)
	require.NoError(t, differentImplicitEnvErr)
	require.Equal(t, key, keyWithDifferentImplicitEnv)
}

func TestPersistentExecutableLifecycleKeyIncludesExplicitEffectiveEnv(t *testing.T) {
	t.Parallel()

	exe := persistentLifecycleKeyTestExecutable()
	exe.Status.EffectiveEnv = []apiv1.EnvVar{
		{Name: "EXPLICIT", Value: "first"},
	}

	key, _, keyErr := persistentExecutableLifecycleKey(exe)
	require.NoError(t, keyErr)

	exeWithDifferentExplicitEnv := persistentLifecycleKeyTestExecutable()
	exeWithDifferentExplicitEnv.Status.EffectiveEnv = []apiv1.EnvVar{
		{Name: "EXPLICIT", Value: "second"},
	}

	keyWithDifferentExplicitEnv, _, differentExplicitEnvErr := persistentExecutableLifecycleKey(exeWithDifferentExplicitEnv)
	require.NoError(t, differentExplicitEnvErr)
	require.NotEqual(t, key, keyWithDifferentExplicitEnv)
}

func TestPersistentExecutableLifecycleKeyIncludesEffectiveArgs(t *testing.T) {
	t.Parallel()

	exe := persistentLifecycleKeyTestExecutable()
	exe.Status.EffectiveArgs = []string{"--port", "5000"}

	key, _, keyErr := persistentExecutableLifecycleKey(exe)
	require.NoError(t, keyErr)

	exeWithDifferentEffectiveArgs := persistentLifecycleKeyTestExecutable()
	exeWithDifferentEffectiveArgs.Status.EffectiveArgs = []string{"--port", "5001"}

	keyWithDifferentEffectiveArgs, _, differentEffectiveArgsErr := persistentExecutableLifecycleKey(exeWithDifferentEffectiveArgs)
	require.NoError(t, differentEffectiveArgsErr)
	require.NotEqual(t, key, keyWithDifferentEffectiveArgs)
}

func TestPersistentExecutableLifecycleKeyPreservesStringBoundaries(t *testing.T) {
	t.Parallel()

	exe := persistentLifecycleKeyTestExecutable()
	exe.Spec.ExecutablePath = "A"
	exe.Spec.WorkingDirectory = "BC"
	exe.Spec.Args = []string{"A", "BC"}

	key, _, keyErr := persistentExecutableLifecycleKey(exe)
	require.NoError(t, keyErr)

	exeWithAmbiguousConcatenation := persistentLifecycleKeyTestExecutable()
	exeWithAmbiguousConcatenation.Spec.ExecutablePath = "AB"
	exeWithAmbiguousConcatenation.Spec.WorkingDirectory = "C"
	exeWithAmbiguousConcatenation.Spec.Args = []string{"AB", "C"}

	keyWithAmbiguousConcatenation, _, ambiguousConcatenationErr := persistentExecutableLifecycleKey(exeWithAmbiguousConcatenation)
	require.NoError(t, ambiguousConcatenationErr)
	require.NotEqual(t, key, keyWithAmbiguousConcatenation)
}

func TestPersistentExecutableLifecycleInfoSanitizesEnvMetadata(t *testing.T) {
	t.Parallel()

	exe := persistentLifecycleKeyTestExecutable()
	exe.Status.EffectiveEnv = []apiv1.EnvVar{
		{Name: "EXPLICIT", Value: "super-secret-value"},
	}

	lifecycleInfo, lifecycleInfoErr := persistentExecutableLifecycleInfo(exe)
	require.NoError(t, lifecycleInfoErr)
	require.True(t, lifecycleInfo.HasDefaultKey)
	require.NotContains(t, lifecycleInfo.Metadata, "super-secret-value")

	var metadata persistentExecutableLifecycleMetadata
	require.NoError(t, json.Unmarshal([]byte(lifecycleInfo.Metadata), &metadata))
	require.Len(t, metadata.ExplicitEffectiveEnv, 1)
	require.Equal(t, "EXPLICIT", metadata.ExplicitEffectiveEnv[0].Name)
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("super-secret-value"))), metadata.ExplicitEffectiveEnv[0].ValueHash)
}

func TestCalculatePersistentExecutableChanges(t *testing.T) {
	t.Parallel()

	oldExe := persistentLifecycleKeyTestExecutable()
	oldExe.Status.EffectiveArgs = []string{"--port", "5000"}
	oldExe.Status.EffectiveEnv = []apiv1.EnvVar{{Name: "EXPLICIT", Value: "first"}}
	oldLifecycleInfo, oldLifecycleInfoErr := persistentExecutableLifecycleInfo(oldExe)
	require.NoError(t, oldLifecycleInfoErr)

	newExe := persistentLifecycleKeyTestExecutable()
	newExe.Status.EffectiveArgs = []string{"--port", "5001"}
	newExe.Status.EffectiveEnv = []apiv1.EnvVar{{Name: "EXPLICIT", Value: "second"}}
	newLifecycleInfo, newLifecycleInfoErr := persistentExecutableLifecycleInfo(newExe)
	require.NoError(t, newLifecycleInfoErr)

	args, env, other := calculatePersistentExecutableChanges(oldLifecycleInfo.Metadata, newLifecycleInfo.Metadata)
	require.Equal(t, []string{"Effective arguments changed"}, args)
	require.ElementsMatch(t, []string{"EXPLICIT"}, env)
	require.Empty(t, other)
}

func persistentLifecycleKeyTestExecutable() *apiv1.Executable {
	return &apiv1.Executable{
		Spec: apiv1.ExecutableSpec{
			ExecutablePath:   "/path/to/app",
			WorkingDirectory: "/path/to/workdir",
			Args: []string{
				"--port",
				"{{ port }}",
			},
			Env: []apiv1.EnvVar{
				{Name: "EXPLICIT", Value: "{{ value }}"},
			},
			Persistent: true,
		},
	}
}
