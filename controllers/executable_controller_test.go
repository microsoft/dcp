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
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/process"
)

func TestExecutableStartResultStringDoesNotFormatZeroIdentityTimeAsTimestamp(t *testing.T) {
	t.Parallel()

	result := NewExecutableStartResult()

	formatted := result.String()

	require.Contains(t, formatted, "processIdentityTime: (empty)")
	require.False(t, strings.Contains(formatted, "0001-01-01"), "zero process identity time should not be formatted as a timestamp")
}

func TestPersistentExecutableLifecycleInfoSanitizesEnvMetadata(t *testing.T) {
	t.Parallel()

	exe := persistentLifecycleKeyTestExecutable()
	exe.Status.EffectiveArgs = exe.Spec.Args
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
	require.Equal(t, []string{"EXPLICIT"}, env)
	require.Empty(t, other)
}

func TestChangedExecutableEnvNamesReturnsSortedNames(t *testing.T) {
	t.Parallel()

	changedNames := changedExecutableEnvNames(
		[]persistentExecutableEnvMetadata{
			{Name: "Z_REMOVED", ValueHash: "removed"},
			{Name: "EXPLICIT", ValueHash: "first"},
			{Name: "UNCHANGED", ValueHash: "same"},
		},
		[]persistentExecutableEnvMetadata{
			{Name: "UNCHANGED", ValueHash: "same"},
			{Name: "EXPLICIT", ValueHash: "second"},
			{Name: "A_ADDED", ValueHash: "added"},
		},
	)

	require.Equal(t, []string{"A_ADDED", "EXPLICIT", "Z_REMOVED"}, changedNames)
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

	change := reconciler.manageExecutable(ctx, exe, logr.Discard())

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

	change := reconciler.manageExecutable(ctx, exe, logr.Discard())

	require.NotEqual(t, noChange, change)
	require.Equal(t, apiv1.ExecutableStateEmpty, exe.Status.State)
	require.Len(t, runner.adoptedRuns, 1)
	require.Equal(t, []RunID{RunID(record.RunID)}, runner.stoppedRuns)
	require.Zero(t, runner.startRunCount)
	_, getErr := store.GetPersistentProcess(ctx, record.ResourceKey)
	require.ErrorIs(t, getErr, statestore.ErrPersistentProcessNotFound)
}

func TestHandleNewPersistentExecutableWaitsWhenLeaseHeld(t *testing.T) {
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
	otherOwner := reconciler.config.ResourceLeaseOwner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(-time.Hour)
	_, acquireErr := store.AcquireResourceLease(ctx, exe, otherOwner, time.Minute)
	require.NoError(t, acquireErr)

	change := reconciler.manageExecutable(ctx, exe, logr.Discard())

	require.Equal(t, additionalReconciliationNeeded, change)
	require.Equal(t, apiv1.ExecutableStateEmpty, exe.Status.State)
	require.Empty(t, runner.adoptedRuns)
	require.Empty(t, runner.stoppedRuns)
	require.Zero(t, runner.startRunCount)
}

func TestHandleNewPersistentExecutableHoldsLeaseDuringStartup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, runner, store := newPersistentExecutableAdoptionTestReconciler(t)
	exe := persistentLifecycleKeyTestExecutable()
	exe.Name = "api"
	exe.Spec.Start = pointersTo(true)
	exe.Spec.Args = nil
	exe.Spec.Env = nil

	change := reconciler.manageExecutable(ctx, exe, logr.Discard())

	require.Equal(t, additionalReconciliationNeeded|statusChanged, change)
	require.Equal(t, 1, runner.startRunCount)
	require.NoError(t, store.VerifyResourceLeaseHeld(ctx, exe, reconciler.config.ResourceLeaseOwner))
}

func TestPersistentExecutableStartupWorkRequiresHeldLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, runner, _ := newPersistentExecutableAdoptionTestReconciler(t)
	exe := persistentLifecycleKeyTestExecutable()
	exe.Name = "api"
	exe.Spec.Start = pointersTo(true)
	exe.Spec.Args = nil
	exe.Spec.Env = nil

	runInfo := NewRunInfo(exe)
	runInfo.RunID = getStartingRunID(exe.NamespacedName())
	runInfo.ExeState = apiv1.ExecutableStateStarting
	reconciler.runs.Store(exe.NamespacedName(), runInfo.RunID, runInfo)

	change := reconciler.startExecutable(ctx, exe, runInfo, logr.Discard())

	require.Equal(t, statusChanged, change)
	require.Equal(t, apiv1.ExecutableStateFailedToStart, exe.Status.State)
	require.Zero(t, runner.startRunCount)
}

func TestSetPersistentExecutableStableStateReleasesLease(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		state apiv1.ExecutableState
	}{
		{name: "running", state: apiv1.ExecutableStateRunning},
		{name: "terminated", state: apiv1.ExecutableStateTerminated},
		{name: "failed to start", state: apiv1.ExecutableStateFailedToStart},
		{name: "finished", state: apiv1.ExecutableStateFinished},
		{name: "unknown", state: apiv1.ExecutableStateUnknown},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			reconciler, _, store := newPersistentExecutableAdoptionTestReconciler(t)
			exe := persistentLifecycleKeyTestExecutable()
			exe.Namespace = "default"
			exe.Name = testCase.name

			_, acquireErr := store.AcquireResourceLease(ctx, exe, reconciler.config.ResourceLeaseOwner, time.Minute)
			require.NoError(t, acquireErr)

			reconciler.setExecutableState(exe, testCase.state)

			verifyErr := store.VerifyResourceLeaseHeld(ctx, exe, reconciler.config.ResourceLeaseOwner)
			require.ErrorIs(t, verifyErr, statestore.ErrResourceLeaseNotHeld)
		})
	}
}

func TestSetPersistentExecutableTransientStateKeepsLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, _, store := newPersistentExecutableAdoptionTestReconciler(t)
	exe := persistentLifecycleKeyTestExecutable()
	exe.Namespace = "default"
	exe.Name = "api"

	_, acquireErr := store.AcquireResourceLease(ctx, exe, reconciler.config.ResourceLeaseOwner, time.Minute)
	require.NoError(t, acquireErr)

	reconciler.setExecutableState(exe, apiv1.ExecutableStateStarting)

	require.NoError(t, store.VerifyResourceLeaseHeld(ctx, exe, reconciler.config.ResourceLeaseOwner))
}

func TestPersistentExecutableDeletionPreservesReusableRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, runner, store := newPersistentExecutableAdoptionTestReconciler(t)
	scheme := apiruntime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	reconciler.Client = fake.NewClientBuilder().WithScheme(scheme).Build()

	exe := persistentLifecycleKeyTestExecutable()
	exe.ObjectMeta = metav1.ObjectMeta{
		Namespace:  "default",
		Name:       "api",
		UID:        "api-uid",
		Finalizers: []string{executableFinalizer},
	}
	exe.Spec.Start = pointersTo(false)
	exe.Spec.Args = []string{"--port", "5000"}
	exe.Spec.Env = []apiv1.EnvVar{{Name: "EXPLICIT", Value: "stable"}}

	record := persistentProcessRecordForTest(t, exe)
	require.NoError(t, store.UpsertPersistentProcess(ctx, record))

	change := reconciler.handleDeletionRequest(ctx, exe, logr.Discard())

	require.Equal(t, metadataChanged, change)
	require.Empty(t, exe.Finalizers)
	require.Empty(t, runner.stoppedRuns)
	require.Zero(t, runner.startRunCount)
	preservedRecord, getErr := store.GetPersistentProcess(ctx, exe.GetLeaseKey())
	require.NoError(t, getErr)
	require.Equal(t, record.ResourceKey, preservedRecord.ResourceKey)
	require.Equal(t, record.LifecycleKey, preservedRecord.LifecycleKey)
	require.Equal(t, record.PID, preservedRecord.PID)
}

func TestRunCompletionSkipsPersistentRecordCleanupForNonPersistentExecutable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconciler, _, store := newPersistentExecutableAdoptionTestReconciler(t)
	exe := persistentLifecycleKeyTestExecutable()
	exe.Spec.Persistent = false
	exe.ObjectMeta = metav1.ObjectMeta{
		Namespace: "default",
		Name:      "api",
		UID:       "api-uid",
	}

	record := persistentProcessRecordForTest(t, exe)
	require.NoError(t, store.UpsertPersistentProcess(ctx, record))

	runID := RunID("run-1")
	runInfo := NewRunInfo(exe)
	runInfo.RunID = runID
	runInfo.ExeState = apiv1.ExecutableStateRunning
	reconciler.runs.Store(exe.NamespacedName(), runID, runInfo)

	reconciler.OnRunCompleted(runID, pointersTo(int32(0)), nil)
	reconciler.runs.RunDeferredOps(exe.NamespacedName(), exe)

	_, preservedErr := store.GetPersistentProcess(ctx, exe.GetLeaseKey())
	require.NoError(t, preservedErr)
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
	releasedRuns  []RunID
}

func (r *persistentExecutableTestRunner) StartRun(context.Context, *apiv1.Executable, RunChangeHandler, logr.Logger) *ExecutableStartResult {
	r.startRunCount++
	return &ExecutableStartResult{ExeState: apiv1.ExecutableStateStarting}
}

func (r *persistentExecutableTestRunner) StopRun(_ context.Context, runID RunID, _ logr.Logger) error {
	r.stoppedRuns = append(r.stoppedRuns, runID)
	return nil
}

func (r *persistentExecutableTestRunner) ReleaseRun(_ context.Context, runID RunID, _ logr.Logger) error {
	r.releasedRuns = append(r.releasedRuns, runID)
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
	leaseOwner, leaseOwnerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, leaseOwnerErr)
	reconciler := &ExecutableReconciler{
		ReconcilerBase: NewReconcilerBase[apiv1.Executable](nil, nil, logr.Discard(), context.Background()),
		ExecutableRunners: map[apiv1.ExecutionType]ExecutableRunner{
			apiv1.ExecutionTypeProcess: runner,
		},
		runs:   NewObjectStateMap[RunID, ExecutableRunInfo, *ExecutableRunInfo, *apiv1.Executable](),
		config: ExecutableReconcilerConfig{StateStore: store, ResourceLeaseOwner: leaseOwner},
	}
	return reconciler, runner, store
}

func persistentProcessRecordForTest(t *testing.T, exe *apiv1.Executable) statestore.PersistentProcessRecord {
	t.Helper()

	currentProcess, currentProcessErr := process.This()
	require.NoError(t, currentProcessErr)
	if len(exe.Status.EffectiveArgs) == 0 {
		exe.Status.EffectiveArgs = append([]string(nil), exe.Spec.Args...)
	}
	if len(exe.Status.EffectiveEnv) == 0 {
		exe.Status.EffectiveEnv = append([]apiv1.EnvVar(nil), exe.Spec.Env...)
	}
	lifecycleInfo, lifecycleInfoErr := persistentExecutableLifecycleInfo(exe)
	require.NoError(t, lifecycleInfoErr)

	return statestore.PersistentProcessRecord{
		ResourceKey:       exe.GetLeaseKey(),
		LifecycleKey:      lifecycleInfo.Key,
		PID:               currentProcess.Pid,
		IdentityTime:      currentProcess.IdentityTime,
		RunID:             fmt.Sprintf("%d", currentProcess.Pid),
		StdOutFile:        filepath.Join(t.TempDir(), "stdout.log"),
		StdErrFile:        filepath.Join(t.TempDir(), "stderr.log"),
		LifecycleMetadata: lifecycleInfo.Metadata,
	}
}

func pointersTo[T any](value T) *T {
	return &value
}
