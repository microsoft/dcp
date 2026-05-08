/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	stdslices "slices"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
)

func (r *ExecutableReconciler) withPersistentExecutableLease(ctx context.Context, exe *apiv1.Executable, log logr.Logger, f func(context.Context, *statestore.ResourceLease) objectChange) objectChange {
	if f == nil {
		log.Error(fmt.Errorf("persistent Executable lease callback cannot be nil"), "Could not acquire persistent Executable resource lease")
		return r.setExecutableState(exe, apiv1.ExecutableStateUnknown)
	}

	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		log.Error(stateStoreErr, "Persistent Executable cannot be reconciled without a state store")
		return r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}
	leaseOwner, leaseOwnerErr := r.getResourceLeaseOwner()
	if leaseOwnerErr != nil {
		log.Error(leaseOwnerErr, "Could not determine persistent Executable resource lease owner")
		return r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	var change objectChange
	leaseErr := stateStore.WithResourceLease(ctx, exe, leaseOwner, resourceLeaseRevalidationInterval, "", func(ctx context.Context, lease *statestore.ResourceLease) error {
		log.V(1).Info("Acquired resource lease", "ResourceKey", lease.ResourceKey)
		change = f(ctx, lease)
		return nil
	})
	if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
		logResourceLeaseHeld(log, leaseErr, exe.GetLeaseKey(), "Persistent Executable is being updated by another DCP instance, retrying")
		return additionalReconciliationNeeded
	}
	if leaseErr != nil {
		log.Error(leaseErr, "Could not acquire persistent Executable resource lease", "ResourceKey", exe.GetLeaseKey())
		return r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	return change
}

func (r *ExecutableReconciler) acquirePersistentExecutableResourceLease(ctx context.Context, exe *apiv1.Executable, log logr.Logger) error {
	if !exe.Spec.Persistent {
		return nil
	}
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	leaseOwner, leaseOwnerErr := r.getResourceLeaseOwner()
	if leaseOwnerErr != nil {
		return leaseOwnerErr
	}

	lease, leaseErr := stateStore.AcquireResourceLease(ctx, exe, leaseOwner, resourceLeaseRevalidationInterval, "")
	if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
		logResourceLeaseHeld(log, leaseErr, exe.GetLeaseKey(), "Persistent Executable is being updated by another DCP instance, retrying")
	}
	if leaseErr != nil {
		return leaseErr
	}

	log.V(1).Info("Acquired resource lease", "ResourceKey", lease.ResourceKey)
	return nil
}

func (r *ExecutableReconciler) verifyPersistentExecutableResourceLeaseHeld(ctx context.Context, exe *apiv1.Executable, log logr.Logger) error {
	if !exe.Spec.Persistent {
		return nil
	}
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	leaseOwner, leaseOwnerErr := r.getResourceLeaseOwner()
	if leaseOwnerErr != nil {
		return leaseOwnerErr
	}

	leaseErr := stateStore.VerifyResourceLeaseHeld(ctx, exe, leaseOwner)
	if leaseErr != nil {
		log.Error(leaseErr, "Cannot continue persistent Executable startup because this DCP instance does not hold the resource lease")
		return leaseErr
	}

	return nil
}

func (r *ExecutableReconciler) releasePersistentExecutableResourceLease(ctx context.Context, exe *apiv1.Executable, log logr.Logger) error {
	if !exe.Spec.Persistent {
		return nil
	}
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	leaseOwner, leaseOwnerErr := r.getResourceLeaseOwner()
	if leaseOwnerErr != nil {
		return leaseOwnerErr
	}

	releaseErr := stateStore.ReleaseResourceLease(ctx, exe, leaseOwner)
	if releaseErr != nil {
		log.Error(releaseErr, "Could not release persistent Executable resource lease")
		return releaseErr
	}

	return nil
}

func persistentExecutableStartupInProgress(runInfo *ExecutableRunInfo) bool {
	return runInfo != nil && runInfo.ExeState == apiv1.ExecutableStateStarting
}

func (r *ExecutableReconciler) tryAdoptExistingPersistentExecutable(ctx context.Context, exe *apiv1.Executable, log logr.Logger) (bool, objectChange) {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		log.Error(stateStoreErr, "Persistent Executable cannot be reconciled without a state store")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	record, recordErr := stateStore.GetPersistentProcess(ctx, exe.GetLeaseKey())
	if errors.Is(recordErr, statestore.ErrPersistentProcessNotFound) {
		return false, noChange
	}
	if recordErr != nil {
		log.Error(recordErr, "Could not read persistent Executable process record", "ResourceKey", exe.GetLeaseKey())
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	ri := NewRunInfo(exe)
	computed, environmentChange := r.computeExecutableEnvironment(ctx, exe, log, ri)
	if !computed {
		return false, environmentChange
	}

	adopted, adoptionChange := r.tryAdoptPersistentExecutableRecord(ctx, exe, ri, record, log)
	return adopted, environmentChange | adoptionChange
}

func (r *ExecutableReconciler) tryAdoptPersistentExecutable(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) (bool, objectChange) {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		log.Error(stateStoreErr, "Persistent Executable cannot be reconciled without a state store")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	resourceKey := exe.GetLeaseKey()
	record, recordErr := stateStore.GetPersistentProcess(ctx, resourceKey)
	if errors.Is(recordErr, statestore.ErrPersistentProcessNotFound) {
		return false, noChange
	}
	if recordErr != nil {
		log.Error(recordErr, "Could not read persistent Executable process record", "ResourceKey", resourceKey)
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	return r.tryAdoptPersistentExecutableRecord(ctx, exe, runInfo, record, log)
}

func (r *ExecutableReconciler) tryAdoptPersistentExecutableRecord(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, record *statestore.PersistentProcessRecord, log logr.Logger) (bool, objectChange) {
	resourceKey := exe.GetLeaseKey()
	lifecycleInfo, lifecycleKeyErr := persistentExecutableLifecycleInfo(exe)
	if lifecycleKeyErr != nil {
		log.Error(lifecycleKeyErr, "Could not calculate Executable lifecycle key")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	if record.LifecycleKey != lifecycleInfo.Key {
		if lifecycleInfo.HasDefaultKey {
			args, env, other := calculatePersistentExecutableChanges(record.LifecycleMetadata, lifecycleInfo.Metadata)
			log.Info("Found existing persistent Executable process record, but calculated lifecycle key does not match",
				"OldLifecycleKey", record.LifecycleKey,
				"NewLifecycleKey", lifecycleInfo.Key,
				"ArgChanges", args,
				"EnvChanges", env,
				"OtherChanges", other)
		} else {
			log.Info("Found existing persistent Executable process record, but custom lifecycle key does not match",
				"OldLifecycleKey", record.LifecycleKey,
				"NewLifecycleKey", lifecycleInfo.Key)
		}
		if _, findErr := process.FindProcess(record.PID, record.IdentityTime); findErr == nil {
			stopErr := r.stopPersistentExecutableRecord(ctx, exe, record, log)
			if stopErr != nil {
				log.Error(stopErr, "Could not stop persistent Executable process with stale lifecycle key", "PID", record.PID)
				return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
			}
		} else {
			log.Info("Persistent Executable process record with stale lifecycle key is no longer running",
				"PID", record.PID,
				"Error", findErr.Error())
		}
		stateStore, stateStoreErr := r.getStateStore()
		if stateStoreErr != nil {
			log.Error(stateStoreErr, "Could not open state store to delete stale persistent Executable process record", "ResourceKey", resourceKey)
			return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		}
		if deleteErr := stateStore.DeletePersistentProcess(ctx, resourceKey); deleteErr != nil {
			log.Error(deleteErr, "Could not delete stale persistent Executable process record", "ResourceKey", resourceKey)
			return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		}
		return false, noChange
	}

	if _, findErr := process.FindProcess(record.PID, record.IdentityTime); findErr != nil {
		log.Info("Persistent Executable process record is stale; process is no longer running",
			"PID", record.PID,
			"Error", findErr.Error())
		stateStore, stateStoreErr := r.getStateStore()
		if stateStoreErr != nil {
			log.Error(stateStoreErr, "Could not open state store to delete stale persistent Executable process record", "ResourceKey", resourceKey)
			return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		}
		if deleteErr := stateStore.DeletePersistentProcess(ctx, resourceKey); deleteErr != nil {
			log.Error(deleteErr, "Could not delete stale persistent Executable process record", "ResourceKey", resourceKey)
			return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
		}
		return false, noChange
	}

	runner, runnerErr := r.getExecutableRunner(exe, StartupStageDefaultRunner)
	if runnerErr != nil {
		log.Error(runnerErr, "The persistent Executable runner is not available")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}
	persistentRunner, ok := runner.(PersistentExecutableRunner)
	if !ok {
		log.Error(fmt.Errorf("runner for execution type %s does not support persistent run adoption", record.ExecutionType), "The persistent Executable cannot be adopted")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	runID := RunID(record.RunID)
	adoptionErr := persistentRunner.AdoptRun(ctx, ExecutableRunAdoptionInfo{
		RunID:               runID,
		Pid:                 record.PID,
		ProcessIdentityTime: record.IdentityTime,
		StdOutFile:          record.StdOutFile,
		StdErrFile:          record.StdErrFile,
		CommandInfo:         exe.Spec.ExecutablePath,
	}, r, log)
	if adoptionErr != nil {
		log.Error(adoptionErr, "Could not adopt persistent Executable process")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	ri := runInfo
	if ri == nil {
		ri = NewRunInfo(exe)
	}
	startingRunID := ri.RunID
	ri.ExeState = apiv1.ExecutableStateRunning
	ri.RunID = runID
	pointers.SetValue(&ri.Pid, int64(record.PID))
	ri.ProcessIdentityTime = record.IdentityTime
	ri.DisplayStartTime = record.DisplayStartTime
	ri.StartupTimestamp = metav1.NewMicroTime(record.DisplayStartTime)
	ri.StdOutFile = record.StdOutFile
	ri.StdErrFile = record.StdErrFile
	ri.startupStage = StartupStageDefaultRunner
	if startingRunID == UnknownRunID {
		r.runs.Store(exe.NamespacedName(), ri.RunID, ri.Clone())
	} else {
		r.runs.UpdateChangingStateKey(exe.NamespacedName(), startingRunID, ri.RunID, ri.Clone())
	}

	log.Info("Adopted persistent Executable process", "PID", record.PID, "RunID", runID)
	return true, ri.ApplyTo(exe, log) | r.setExecutableState(exe, apiv1.ExecutableStateRunning)
}

func (r *ExecutableReconciler) stopPersistentExecutableRecord(ctx context.Context, exe *apiv1.Executable, record *statestore.PersistentProcessRecord, log logr.Logger) error {
	runner, runnerErr := r.getExecutableRunner(exe, StartupStageDefaultRunner)
	if runnerErr != nil {
		return fmt.Errorf("persistent Executable runner is not available: %w", runnerErr)
	}
	persistentRunner, ok := runner.(PersistentExecutableRunner)
	if !ok {
		return fmt.Errorf("runner for execution type %s does not support persistent run adoption", record.ExecutionType)
	}

	runID := RunID(record.RunID)
	adoptionErr := persistentRunner.AdoptRun(ctx, ExecutableRunAdoptionInfo{
		RunID:               runID,
		Pid:                 record.PID,
		ProcessIdentityTime: record.IdentityTime,
		StdOutFile:          record.StdOutFile,
		StdErrFile:          record.StdErrFile,
		CommandInfo:         exe.Spec.ExecutablePath,
	}, nil, log)
	if adoptionErr != nil {
		return fmt.Errorf("could not adopt persistent Executable process before stopping it: %w", adoptionErr)
	}

	return runner.StopRun(ctx, runID, log)
}

func (r *ExecutableReconciler) upsertPersistentProcessRecord(ctx context.Context, exe *apiv1.Executable, res *ExecutableStartResult) error {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	if res.Pid == apiv1.UnknownPID || res.Pid == nil {
		return fmt.Errorf("cannot persist Executable process record without a valid PID")
	}

	pid, pidErr := process.Int64_ToPidT(*res.Pid)
	if pidErr != nil {
		return fmt.Errorf("cannot persist Executable process record with invalid PID %d: %w", *res.Pid, pidErr)
	}

	lifecycleInfo, lifecycleKeyErr := persistentExecutableLifecycleInfo(exe)
	if lifecycleKeyErr != nil {
		return fmt.Errorf("could not calculate Executable lifecycle key: %w", lifecycleKeyErr)
	}

	displayStartTime := res.DisplayStartTime
	if displayStartTime.IsZero() {
		displayStartTime = res.CompletionTimestamp.Time
	}
	executionType := exe.Spec.ExecutionType
	if executionType == "" {
		executionType = apiv1.ExecutionTypeProcess
	}

	return stateStore.UpsertPersistentProcess(ctx, statestore.PersistentProcessRecord{
		ResourceKey:       exe.GetLeaseKey(),
		Name:              exe.NamespacedName(),
		UID:               exe.UID,
		LifecycleKey:      lifecycleInfo.Key,
		PID:               pid,
		IdentityTime:      res.ProcessIdentityTime,
		DisplayStartTime:  displayStartTime,
		RunID:             string(res.RunID),
		StdOutFile:        res.StdOutFile,
		StdErrFile:        res.StdErrFile,
		ExecutionType:     string(executionType),
		LifecycleMetadata: lifecycleInfo.Metadata,
	})
}

type persistentExecutableLifecycleData struct {
	Key           string
	HasDefaultKey bool
	Metadata      string
}

type persistentExecutableLifecycleMetadata struct {
	ExecutablePath                 string                            `json:"executablePath"`
	WorkingDirectory               string                            `json:"workingDirectory,omitempty"`
	ExecutionType                  string                            `json:"executionType,omitempty"`
	AmbientEnvironment             string                            `json:"ambientEnvironment,omitempty"`
	EffectiveArgs                  []string                          `json:"effectiveArgs,omitempty"`
	ExplicitEffectiveEnv           []persistentExecutableEnvMetadata `json:"explicitEffectiveEnv,omitempty"`
	PEMCertificates                []string                          `json:"pemCertificates,omitempty"`
	PEMCertificatesContinueOnError bool                              `json:"pemCertificatesContinueOnError,omitempty"`
}

type persistentExecutableEnvMetadata struct {
	Name      string `json:"name"`
	ValueHash string `json:"valueHash"`
}

func persistentExecutableLifecycleInfo(exe *apiv1.Executable) (persistentExecutableLifecycleData, error) {
	lifecycleKey, hasDefaultLifecycleKey, lifecycleKeyErr := exe.GetLifecycleKey()
	if lifecycleKeyErr != nil {
		return persistentExecutableLifecycleData{}, lifecycleKeyErr
	}
	if !hasDefaultLifecycleKey {
		return persistentExecutableLifecycleData{
			Key:           lifecycleKey,
			HasDefaultKey: false,
		}, nil
	}

	lifecycleSpec, lifecycleSpecErr := exe.EffectiveLifecycleSpec()
	if lifecycleSpecErr != nil {
		return persistentExecutableLifecycleData{}, lifecycleSpecErr
	}
	lifecycleMetadata := persistentExecutableLifecycleMetadata{
		ExecutablePath:     lifecycleSpec.ExecutablePath,
		WorkingDirectory:   lifecycleSpec.WorkingDirectory,
		ExecutionType:      string(lifecycleSpec.ExecutionType),
		AmbientEnvironment: string(lifecycleSpec.AmbientEnvironment.Behavior),
		EffectiveArgs:      stdslices.Clone(lifecycleSpec.Args),
	}

	lifecycleMetadata.ExplicitEffectiveEnv = make([]persistentExecutableEnvMetadata, 0, len(lifecycleSpec.Env))
	for _, envVar := range lifecycleSpec.Env {
		lifecycleMetadata.ExplicitEffectiveEnv = append(lifecycleMetadata.ExplicitEffectiveEnv, persistentExecutableEnvMetadata{
			Name:      envVar.Name,
			ValueHash: fmt.Sprintf("%x", sha256.Sum256([]byte(envVar.Value))),
		})
	}

	if lifecycleSpec.PemCertificates != nil {
		sortedPemCertificates := stdslices.Clone(lifecycleSpec.PemCertificates.Certificates)
		stdslices.SortFunc(sortedPemCertificates, func(c1, c2 apiv1.PemCertificate) int {
			return strings.Compare(c1.Thumbprint, c2.Thumbprint)
		})

		for i := range sortedPemCertificates {
			lifecycleMetadata.PEMCertificates = append(lifecycleMetadata.PEMCertificates, sortedPemCertificates[i].Thumbprint)
		}
		lifecycleMetadata.PEMCertificatesContinueOnError = lifecycleSpec.PemCertificates.ContinueOnError
	}

	metadataBytes, metadataErr := json.Marshal(lifecycleMetadata)
	if metadataErr != nil {
		return persistentExecutableLifecycleData{}, metadataErr
	}

	return persistentExecutableLifecycleData{
		Key:           lifecycleKey,
		HasDefaultKey: true,
		Metadata:      string(metadataBytes),
	}, nil
}

func calculatePersistentExecutableChanges(oldMetadata, newMetadata string) (args []string, env []string, other []string) {
	if oldMetadata == "" {
		return nil, nil, []string{"Executable lifecycle metadata was not recorded for the existing process"}
	}

	var oldLifecycleMetadata persistentExecutableLifecycleMetadata
	var newLifecycleMetadata persistentExecutableLifecycleMetadata
	oldMetadataErr := json.Unmarshal([]byte(oldMetadata), &oldLifecycleMetadata)
	newMetadataErr := json.Unmarshal([]byte(newMetadata), &newLifecycleMetadata)
	if oldMetadataErr != nil || newMetadataErr != nil {
		return nil, nil, []string{"Executable lifecycle metadata could not be decoded"}
	}

	if !reflect.DeepEqual(oldLifecycleMetadata.EffectiveArgs, newLifecycleMetadata.EffectiveArgs) {
		args = append(args, "Effective arguments changed")
	}

	env = changedExecutableEnvNames(oldLifecycleMetadata.ExplicitEffectiveEnv, newLifecycleMetadata.ExplicitEffectiveEnv)

	if oldLifecycleMetadata.ExecutablePath != newLifecycleMetadata.ExecutablePath {
		other = append(other, "Executable path changed")
	}
	if oldLifecycleMetadata.WorkingDirectory != newLifecycleMetadata.WorkingDirectory {
		other = append(other, "Working directory changed")
	}
	if oldLifecycleMetadata.ExecutionType != newLifecycleMetadata.ExecutionType {
		other = append(other, "Execution type changed")
	}
	if oldLifecycleMetadata.AmbientEnvironment != newLifecycleMetadata.AmbientEnvironment {
		other = append(other, "Ambient environment behavior changed")
	}
	if !reflect.DeepEqual(oldLifecycleMetadata.PEMCertificates, newLifecycleMetadata.PEMCertificates) ||
		oldLifecycleMetadata.PEMCertificatesContinueOnError != newLifecycleMetadata.PEMCertificatesContinueOnError {
		other = append(other, "Executable PEM certificates entries changed")
	}

	return args, env, other
}

func changedExecutableEnvNames(oldEnv, newEnv []persistentExecutableEnvMetadata) []string {
	oldEnvByName := map[string]string{}
	newEnvByName := map[string]string{}
	for _, envVar := range oldEnv {
		oldEnvByName[envVar.Name] = envVar.ValueHash
	}
	for _, envVar := range newEnv {
		newEnvByName[envVar.Name] = envVar.ValueHash
	}

	changedEnv := map[string]bool{}
	for name, oldValueHash := range oldEnvByName {
		if newValueHash, found := newEnvByName[name]; !found || oldValueHash != newValueHash {
			changedEnv[name] = true
		}
	}
	for name := range newEnvByName {
		if _, found := oldEnvByName[name]; !found {
			changedEnv[name] = true
		}
	}

	return maps.Keys(changedEnv)
}

func (r *ExecutableReconciler) deletePersistentProcessRecord(ctx context.Context, name types.NamespacedName, log logr.Logger) {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return
	}

	deleteErr := stateStore.DeletePersistentProcess(ctx, name.String())
	if deleteErr != nil {
		log.Error(deleteErr, "Could not delete persistent Executable process record", "Executable", name.String())
	}
}

func (r *ExecutableReconciler) deletePersistentProcessRecordWithLease(ctx context.Context, name types.NamespacedName, log logr.Logger) {
	exe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: name.Namespace,
			Name:      name.Name,
		},
		Spec: apiv1.ExecutableSpec{
			Persistent: true,
		},
	}
	_ = r.withPersistentExecutableLease(ctx, exe, log, func(ctx context.Context, _ *statestore.ResourceLease) objectChange {
		r.deletePersistentProcessRecord(ctx, name, log)
		return noChange
	})
}

func (r *ExecutableReconciler) getStateStore() (*statestore.Store, error) {
	if r.config.StateStore == nil {
		return nil, fmt.Errorf("state store is not configured")
	}
	return r.config.StateStore, nil
}

func (r *ExecutableReconciler) getResourceLeaseOwner() (process.ProcessTreeItem, error) {
	if r.config.ResourceLeaseOwner.Pid > 0 && !r.config.ResourceLeaseOwner.IdentityTime.IsZero() {
		return r.config.ResourceLeaseOwner, nil
	}
	return statestore.CurrentResourceLeaseOwner()
}

func (r *ExecutableReconciler) releasePersistentExecutableResources(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, log logr.Logger) {
	r.disableEndpointsAndHealthProbes(ctx, exe, runInfo, log)
	logger.ReleaseResourceLog(exe.GetResourceId())
	if runInfo != nil && runInfo.RunID != UnknownRunID {
		runner, runnerErr := r.getExecutableRunner(exe, runInfo.startupStage)
		if runnerErr != nil {
			log.Error(runnerErr, "Could not release persistent Executable runner tracking", "RunID", runInfo.RunID)
		} else if releaseErr := runner.ReleaseRun(ctx, runInfo.RunID, log); releaseErr != nil {
			log.Error(releaseErr, "Could not release persistent Executable runner tracking", "RunID", runInfo.RunID)
		}
	}
	if r.persistentProcessRecordCanBeReused(ctx, exe, log) {
		log.V(1).Info("Preserving persistent Executable process record for reuse", "ResourceKey", exe.GetLeaseKey())
		return
	}
	r.deletePersistentProcessRecord(ctx, exe.NamespacedName(), log)
}

func (r *ExecutableReconciler) persistentProcessRecordCanBeReused(ctx context.Context, exe *apiv1.Executable, log logr.Logger) bool {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		log.Error(stateStoreErr, "Could not determine whether persistent Executable process record can be reused", "ResourceKey", exe.GetLeaseKey())
		return false
	}

	record, recordErr := stateStore.GetPersistentProcess(ctx, exe.GetLeaseKey())
	if errors.Is(recordErr, statestore.ErrPersistentProcessNotFound) {
		return false
	}
	if recordErr != nil {
		log.Error(recordErr, "Could not read persistent Executable process record", "ResourceKey", exe.GetLeaseKey())
		return false
	}

	lifecycleInfo, lifecycleKeyErr := persistentExecutableLifecycleInfo(exe)
	if lifecycleKeyErr != nil {
		log.Error(lifecycleKeyErr, "Could not calculate Executable lifecycle key")
		return false
	}
	if record.LifecycleKey != lifecycleInfo.Key {
		return false
	}

	if _, findErr := process.FindProcess(record.PID, record.IdentityTime); findErr != nil {
		log.Info("Persistent Executable process record is not reusable because the process is no longer running",
			"PID", record.PID,
			"Error", findErr.Error())
		return false
	}

	return true
}
