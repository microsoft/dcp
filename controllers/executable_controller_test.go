/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/process"
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

func TestHandleNewPersistentExecutableWithStartFalseAdoptsMatchingRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, runner, store := newPersistentExecutableAdoptionTestReconciler(t)
	exe := persistentLifecycleKeyTestExecutable()
	exe.Name = "api"
	exe.Spec.Start = pointersTo(false)
	exe.Spec.Args = []string{"--port", "5000"}
	exe.Spec.Env = []apiv1.EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	record := persistentProcessRecordForTest(t, exe)
	require.NoError(t, store.UpsertPersistentProcess(ctx, record))

	change := handleNewExecutable(ctx, reconciler, exe, apiv1.ExecutableStateEmpty, nil, logr.Discard())

	require.NotEqual(t, noChange, change)
	require.Equal(t, apiv1.ExecutableStateRunning, exe.Status.State)
	require.Equal(t, int64(record.PID), *exe.Status.PID)
	require.Len(t, runner.adoptedRuns, 1)
	require.Empty(t, runner.stoppedRuns)
	require.Zero(t, runner.startRunCount)
}

func TestHandleNewPersistentExecutableWithStartFalseStopsMismatchedRecordAndWaits(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, runner, store := newPersistentExecutableAdoptionTestReconciler(t)
	exe := persistentLifecycleKeyTestExecutable()
	exe.Name = "api"
	exe.Spec.Start = pointersTo(false)
	exe.Spec.Args = []string{"--port", "5000"}
	exe.Spec.Env = []apiv1.EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	record := persistentProcessRecordForTest(t, exe)
	record.LifecycleKey = "stale"
	require.NoError(t, store.UpsertPersistentProcess(ctx, record))

	change := handleNewExecutable(ctx, reconciler, exe, apiv1.ExecutableStateEmpty, nil, logr.Discard())

	require.NotEqual(t, noChange, change)
	require.Equal(t, apiv1.ExecutableStateEmpty, exe.Status.State)
	require.Len(t, runner.adoptedRuns, 1)
	require.Equal(t, []RunID{RunID(record.RunID)}, runner.stoppedRuns)
	require.Zero(t, runner.startRunCount)
	_, getErr := store.GetPersistentProcess(ctx, record.ResourceKey)
	require.ErrorIs(t, getErr, statestore.ErrPersistentProcessNotFound)
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

type persistentExecutableTestRunner struct {
	startRunCount int
	adoptedRuns   []ExecutableRunAdoptionInfo
	stoppedRuns   []RunID
}

func (r *persistentExecutableTestRunner) StartRun(context.Context, *apiv1.Executable, RunChangeHandler, logr.Logger) *ExecutableStartResult {
	r.startRunCount++
	return &ExecutableStartResult{ExeState: apiv1.ExecutableStateStarting}
}

func (r *persistentExecutableTestRunner) StopRun(_ context.Context, runID RunID, _ logr.Logger) error {
	r.stoppedRuns = append(r.stoppedRuns, runID)
	return nil
}

func (r *persistentExecutableTestRunner) AdoptRun(_ context.Context, run ExecutableRunAdoptionInfo, _ RunChangeHandler, _ logr.Logger) error {
	r.adoptedRuns = append(r.adoptedRuns, run)
	return nil
}

func newPersistentExecutableAdoptionTestReconciler(t *testing.T) (*ExecutableReconciler, *persistentExecutableTestRunner, *statestore.Store) {
	t.Helper()

	store, storeErr := statestore.Open(context.Background(), statestore.Options{
		Path: filepath.Join(t.TempDir(), "state.sqlite3"),
	})
	require.NoError(t, storeErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	runner := &persistentExecutableTestRunner{}
	reconciler := &ExecutableReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.Executable](nil, nil, logr.Discard(), context.Background()),
		ExecutableRunners: map[apiv1.ExecutionType]ExecutableRunner{
			apiv1.ExecutionTypeProcess: runner,
		},
		runs:   NewObjectStateMap[RunID, ExecutableRunInfo, *ExecutableRunInfo, *apiv1.Executable](),
		config: ExecutableReconcilerConfig{StateStore: store},
	}
	return reconciler, runner, store
}

func persistentProcessRecordForTest(t *testing.T, exe *apiv1.Executable) statestore.PersistentProcessRecord {
	t.Helper()

	currentProcess, currentProcessErr := process.This()
	require.NoError(t, currentProcessErr)
	lifecycleInfo, lifecycleInfoErr := persistentExecutableLifecycleInfo(exe)
	require.NoError(t, lifecycleInfoErr)

	return statestore.PersistentProcessRecord{
		ResourceKey:       persistentExecutableResourceKey(exe.NamespacedName()),
		Name:              exe.NamespacedName(),
		UID:               exe.UID,
		LifecycleKey:      lifecycleInfo.Key,
		PID:               currentProcess.Pid,
		IdentityTime:      currentProcess.IdentityTime,
		DisplayStartTime:  time.Now().UTC(),
		RunID:             fmt.Sprintf("%d", currentProcess.Pid),
		StdOutFile:        filepath.Join(t.TempDir(), "stdout.log"),
		StdErrFile:        filepath.Join(t.TempDir(), "stderr.log"),
		ExecutionType:     string(apiv1.ExecutionTypeProcess),
		LifecycleMetadata: lifecycleInfo.Metadata,
	}
}

func pointersTo[T any](value T) *T {
	return &value
}
