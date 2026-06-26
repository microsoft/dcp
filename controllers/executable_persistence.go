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

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
)

func (r *ExecutableReconciler) acquirePersistentExecutableResourceLeaseAndRecord(ctx context.Context, exe *apiv1.Executable, log logr.Logger) (*statestore.ResourceLease, *statestore.PersistentProcessRecord, error) {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return nil, nil, stateStoreErr
	}
	lease, leaseErr := stateStore.AcquireResourceLease(ctx, exe, r.config.ResourceLeaseOwner, resourceLeaseRevalidationInterval)
	if errors.Is(leaseErr, statestore.ErrResourceLeaseHeld) {
		logResourceLeaseHeld(log, leaseErr, exe.GetLeaseKey(), "Persistent Executable is being updated by another DCP instance, retrying")
	}
	if leaseErr != nil {
		return nil, nil, leaseErr
	}

	log.V(1).Info("Acquired resource lease", "ResourceKey", lease.ResourceKey)
	record, recordErr := stateStore.GetPersistentProcess(ctx, exe.GetLeaseKey())
	if errors.Is(recordErr, statestore.ErrPersistentProcessNotFound) {
		return lease, nil, nil
	}
	if recordErr != nil {
		if releaseErr := lease.Release(ctx); releaseErr != nil {
			log.Error(releaseErr, "Could not release persistent Executable resource lease after failing to read process record")
		}
		return nil, nil, recordErr
	}

	return lease, record, nil
}

func (r *ExecutableReconciler) verifyPersistentExecutableResourceLeaseHeld(ctx context.Context, exe *apiv1.Executable, log logr.Logger) error {
	if !exe.Spec.EffectiveMode().ShouldReuseExisting() {
		return nil
	}
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	leaseErr := stateStore.VerifyResourceLeaseHeld(ctx, exe, r.config.ResourceLeaseOwner)
	if leaseErr != nil {
		log.Error(leaseErr, "Cannot continue persistent Executable startup because this DCP instance does not hold the resource lease")
		return leaseErr
	}

	return nil
}

func (r *ExecutableReconciler) releasePersistentExecutableResourceLease(
	ctx context.Context,
	exe *apiv1.Executable,
	log logr.Logger,
	suppressNotHeldLog bool,
) error {
	if !exe.Spec.EffectiveMode().ShouldReuseExisting() {
		return nil
	}
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	releaseErr := stateStore.ReleaseResourceLease(ctx, exe, r.config.ResourceLeaseOwner)
	if releaseErr != nil {
		if errors.Is(releaseErr, statestore.ErrResourceLeaseNotHeld) {
			logResourceLeaseNotHeld(log, suppressNotHeldLog, exe.GetLeaseKey(), "Persistent Executable resource lease was not held")
			return releaseErr
		}
		log.Error(releaseErr, "Could not release persistent Executable resource lease")
		return releaseErr
	}

	return nil
}

func (r *ExecutableReconciler) adoptPersistentExecutableRecord(ctx context.Context, exe *apiv1.Executable, runInfo *ExecutableRunInfo, record *statestore.PersistentProcessRecord, persistentRunner PersistentExecutableRunner, log logr.Logger) (bool, objectChange) {
	displayStartTime := process.StartTimeForProcess(record.PID)

	runID := RunID(record.RunID)
	adoptionErr := persistentRunner.AdoptRun(ctx, exe, record, r, log)
	if adoptionErr != nil {
		log.Error(adoptionErr, "Could not adopt persistent Executable process")
		return false, r.setExecutableState(exe, apiv1.ExecutableStateFailedToStart)
	}

	runInfo.ExeState = apiv1.ExecutableStateRunning
	runInfo.RunID = runID
	pointers.SetValue(&runInfo.Pid, int64(record.PID))
	runInfo.ProcessIdentityTime = record.IdentityTime
	runInfo.StartupTimestamp = metav1.NewMicroTime(displayStartTime)
	runInfo.StdOutFile = record.StdOutFile
	runInfo.StdErrFile = record.StdErrFile
	runInfo.startupStage = StartupStageDefaultRunner
	r.runs.Store(exe.NamespacedName(), runInfo.RunID, runInfo.Clone())

	log.Info("Adopted persistent Executable process", "PID", record.PID, "RunID", runID)
	return true, runInfo.ApplyTo(exe, log) | r.setExecutableState(exe, apiv1.ExecutableStateRunning)
}

func (r *ExecutableReconciler) upsertPersistentProcessRecord(ctx context.Context, exe *apiv1.Executable, res *ExecutableStartResult) error {
	stateStore, stateStoreErr := r.getStateStore()
	if stateStoreErr != nil {
		return stateStoreErr
	}
	if res.Pid == apiv1.UnknownPID {
		return fmt.Errorf("cannot persist Executable process record without a valid PID")
	}

	pid, pidErr := process.Int64_ToPidT(*res.Pid)
	if pidErr != nil {
		return fmt.Errorf("cannot persist Executable process record with invalid PID %d: %w", *res.Pid, pidErr)
	}

	record := statestore.PersistentProcessRecord{
		ResourceKey:  exe.GetLeaseKey(),
		PID:          pid,
		IdentityTime: res.ProcessIdentityTime,
		RunID:        string(res.RunID),
		StdOutFile:   res.StdOutFile,
		StdErrFile:   res.StdErrFile,
	}
	lifecycleKey, hasDefaultLifecycleKey, lifecycleKeyErr := exe.GetLifecycleKey()
	if lifecycleKeyErr != nil {
		return fmt.Errorf("cannot calculate Executable lifecycle key: %w", lifecycleKeyErr)
	}
	record.LifecycleKey = lifecycleKey
	if hasDefaultLifecycleKey {
		lifecycleMetadata, lifecycleMetadataErr := persistentExecutableLifecycleMetadataFor(exe)
		if lifecycleMetadataErr != nil {
			return fmt.Errorf("cannot calculate Executable lifecycle metadata: %w", lifecycleMetadataErr)
		}
		record.LifecycleMetadata = lifecycleMetadata
	}

	return stateStore.UpsertPersistentProcess(ctx, record)
}

type persistentExecutableLifecycleMetadata struct {
	ExecutablePath                 string            `json:"executablePath"`
	WorkingDirectory               string            `json:"workingDirectory,omitempty"`
	AmbientEnvironment             string            `json:"ambientEnvironment,omitempty"`
	EffectiveArgs                  []string          `json:"effectiveArgs,omitempty"`
	ExplicitEffectiveEnv           map[string]string `json:"explicitEffectiveEnv,omitempty"`
	PEMCertificates                []string          `json:"pemCertificates,omitempty"`
	PEMCertificatesContinueOnError bool              `json:"pemCertificatesContinueOnError,omitempty"`
}

func persistentExecutableLifecycleMetadataFor(exe *apiv1.Executable) (string, error) {
	lifecycleSpec, lifecycleSpecErr := exe.EffectiveLifecycleSpec()
	if lifecycleSpecErr != nil {
		return "", lifecycleSpecErr
	}
	metadata := persistentExecutableLifecycleMetadata{
		ExecutablePath:     lifecycleSpec.ExecutablePath,
		WorkingDirectory:   lifecycleSpec.WorkingDirectory,
		AmbientEnvironment: string(lifecycleSpec.AmbientEnvironment.Behavior),
		EffectiveArgs:      stdslices.Clone(lifecycleSpec.Args),
	}

	metadata.ExplicitEffectiveEnv = make(map[string]string, len(lifecycleSpec.Env))
	for _, envVar := range lifecycleSpec.Env {
		metadata.ExplicitEffectiveEnv[envVar.Name] = fmt.Sprintf("%x", sha256.Sum256([]byte(envVar.Value)))
	}

	if lifecycleSpec.PemCertificates != nil {
		sortedPemCertificates := stdslices.Clone(lifecycleSpec.PemCertificates.Certificates)
		stdslices.SortFunc(sortedPemCertificates, func(c1, c2 apiv1.PemCertificate) int {
			return strings.Compare(c1.Thumbprint, c2.Thumbprint)
		})
		for i := range sortedPemCertificates {
			metadata.PEMCertificates = append(metadata.PEMCertificates, sortedPemCertificates[i].Thumbprint)
		}
		metadata.PEMCertificatesContinueOnError = lifecycleSpec.PemCertificates.ContinueOnError
	}

	metadataBytes, metadataErr := json.Marshal(metadata)
	if metadataErr != nil {
		return "", metadataErr
	}

	return string(metadataBytes), nil
}

func calculatePersistentExecutableChanges(exe *apiv1.Executable, oldMetadata string) (args []string, env []string, other []string) {
	if oldMetadata == "" {
		return nil, nil, []string{"Executable lifecycle metadata was not recorded for the existing process"}
	}

	newMetadata, newMetadataErr := persistentExecutableLifecycleMetadataFor(exe)
	if newMetadataErr != nil {
		return nil, nil, []string{"Executable lifecycle metadata could not be calculated"}
	}

	var oldLifecycleMetadata persistentExecutableLifecycleMetadata
	var newLifecycleMetadata persistentExecutableLifecycleMetadata
	oldMetadataErr := json.Unmarshal([]byte(oldMetadata), &oldLifecycleMetadata)
	newMetadataDecodeErr := json.Unmarshal([]byte(newMetadata), &newLifecycleMetadata)
	if oldMetadataErr != nil || newMetadataDecodeErr != nil {
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
	if oldLifecycleMetadata.AmbientEnvironment != newLifecycleMetadata.AmbientEnvironment {
		other = append(other, "Ambient environment behavior changed")
	}
	if !reflect.DeepEqual(oldLifecycleMetadata.PEMCertificates, newLifecycleMetadata.PEMCertificates) ||
		oldLifecycleMetadata.PEMCertificatesContinueOnError != newLifecycleMetadata.PEMCertificatesContinueOnError {
		other = append(other, "Executable PEM certificates entries changed")
	}

	return args, env, other
}

func changedExecutableEnvNames(oldEnv, newEnv map[string]string) []string {
	changedEnv := map[string]bool{}
	for name, oldValueHash := range oldEnv {
		if newValueHash, found := newEnv[name]; !found || oldValueHash != newValueHash {
			changedEnv[name] = true
		}
	}
	for name := range newEnv {
		if _, found := oldEnv[name]; !found {
			changedEnv[name] = true
		}
	}

	changedNames := maps.Keys(changedEnv)
	stdslices.Sort(changedNames)
	return changedNames
}

func (r *ExecutableReconciler) getStateStore() (*statestore.Store, error) {
	if r.config.StateStore == nil {
		return nil, fmt.Errorf("state store is not configured")
	}
	return r.config.StateStore, nil
}

func (r *ExecutableReconciler) getPersistentExecutableRunner(exe *apiv1.Executable) (PersistentExecutableRunner, error) {
	runner, runnerErr := r.getExecutableRunner(exe, StartupStageDefaultRunner)
	if runnerErr != nil {
		return nil, fmt.Errorf("persistent Executable runner is not available: %w", runnerErr)
	}
	persistentRunner, ok := runner.(PersistentExecutableRunner)
	if !ok {
		return nil, fmt.Errorf("runner does not support persistent Executable process operations")
	}

	return persistentRunner, nil
}
