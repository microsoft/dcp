/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/syncmap"
)

var errContainerVolumesRecovering = errors.New("container volumes are recovering from a failed reset")

// ContainerVolumeResetRecovery retains verified ownership while reset removes and recreates volumes.
// Share one instance between the Container and ContainerVolume reconcilers in a controller manager.
type ContainerVolumeResetRecovery struct {
	volumes syncmap.Map[string, *containerVolumeRecovery]
	notify  func(types.NamespacedName)
}

type containerVolumeRecovery struct {
	lock           sync.Mutex
	volume         *apiv1.ContainerVolume
	labels         map[string]string
	repairRequired bool
}

func (recovery *ContainerVolumeResetRecovery) begin(volume *apiv1.ContainerVolume, labels map[string]string) (*containerVolumeRecovery, error) {
	if recovery == nil || recovery.notify == nil {
		return nil, fmt.Errorf("container volume reset recovery is not configured")
	}
	record := &containerVolumeRecovery{volume: volume.DeepCopy(), labels: maps.Clone(labels)}
	record.lock.Lock()
	_, found := recovery.volumes.LoadOrStore(volume.Spec.Name, record)
	if found {
		record.lock.Unlock()
		return nil, fmt.Errorf("volume %q has an unfinished reset or recovery", volume.Spec.Name)
	}
	return record, nil
}

func (recovery *ContainerVolumeResetRecovery) finish(record *containerVolumeRecovery) {
	if !record.repairRequired {
		recovery.volumes.Delete(record.volume.Spec.Name)
	}
	record.lock.Unlock()
	recovery.notify(record.volume.NamespacedName())
}

func (r *ContainerReconciler) checkVolumeResetRecovery(ctx context.Context, spec *apiv1.ContainerSpec) error {
	volumeObjects := &apiv1.ContainerVolumeList{}
	hasNamedMount := false
	for _, mount := range spec.VolumeMounts {
		hasNamedMount = hasNamedMount || mount.Type == apiv1.NamedVolumeMount
	}
	if !hasNamedMount {
		return nil
	}
	if listErr := r.NoCacheClient.List(ctx, volumeObjects); listErr != nil {
		return fmt.Errorf("list container volume startup dependencies: %w", listErr)
	}
	for _, mount := range spec.VolumeMounts {
		if mount.Type != apiv1.NamedVolumeMount {
			continue
		}
		if r.config.VolumeResetRecovery != nil {
			if _, found := r.config.VolumeResetRecovery.volumes.Load(mount.Source); found {
				return fmt.Errorf("%w: %s", errContainerVolumesRecovering, mount.Source)
			}
		}
		var record *statestore.PersistentVolumeRecord
		if r.config.StateStore != nil && r.config.WorkloadID != "" {
			volumeKey := (&apiv1.ContainerVolume{Spec: apiv1.ContainerVolumeSpec{Name: mount.Source}}).GetLeaseKey()
			stored, getRecordErr := r.config.StateStore.GetPersistentVolume(ctx, volumeKey)
			if getRecordErr != nil && !errors.Is(getRecordErr, statestore.ErrPersistentVolumeNotFound) {
				return fmt.Errorf("get volume %q startup ownership: %w", mount.Source, getRecordErr)
			}
			if getRecordErr == nil && stored.WorkloadID == r.config.WorkloadID {
				record = stored
			}
		}
		managed := record != nil
		for _, volume := range volumeObjects.Items {
			managed = managed || volume.Spec.Name == mount.Source
		}
		if !managed {
			continue
		}
		inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, mount.Source)
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return fmt.Errorf("%w: managed volume %q is missing", errContainerVolumesRecovering, mount.Source)
		}
		if inspectErr != nil {
			return fmt.Errorf("inspect managed volume %q before container startup: %w", mount.Source, inspectErr)
		}
		if record != nil && (record.RuntimeName != r.orchestrator.Name() ||
			record.VolumeName != mount.Source || !persistentVolumeOwnershipMatches(inspected, record.OwnershipToken)) {
			return fmt.Errorf("managed volume %q ownership does not match its persistent record", mount.Source)
		}
	}
	return nil
}

func (r *VolumeReconciler) recoverResetVolume(ctx context.Context, vol *apiv1.ContainerVolume, record *containerVolumeRecovery, log logr.Logger) objectChange {
	if vol.UID != record.volume.UID {
		replacedErr := fmt.Errorf("volume %q resource was replaced during reset recovery", vol.Spec.Name)
		log.Error(replacedErr, "Could not recover reset volume")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}
	_, data := r.volumeData.BorrowByNamespacedName(vol.NamespacedName())
	if data == nil {
		data = &containerVolumeData{}
	}
	data.state = apiv1.ContainerVolumeStatePending
	r.volumeData.Store(vol.NamespacedName(), volumeName(vol.Spec.Name), data)

	if !r.orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed).IsHealthy() {
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStateRuntimeUnhealthy) | additionalReconciliationNeeded
	}
	if token := record.labels[containers.VolumeOwnershipTokenLabel]; token != "" {
		if r.config.StateStore == nil {
			log.Error(fmt.Errorf("state store is not configured"), "Could not verify reset volume recovery ownership")
			return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
		}
		persistentRecord, getRecordErr := r.config.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
		if getRecordErr != nil {
			log.Error(getRecordErr, "Could not verify reset volume recovery ownership")
			return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
		}
		if persistentRecord.OwnershipToken != token || persistentRecord.WorkloadID != r.config.WorkloadID ||
			persistentRecord.VolumeName != vol.Spec.Name || persistentRecord.RuntimeName != r.orchestrator.Name() {
			log.Error(fmt.Errorf("volume ownership record changed"), "Could not recover reset volume")
			return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
		}
	} else if record.labels[uidLabel] != string(vol.UID) {
		log.Error(fmt.Errorf("volume ownership label does not match its resource"), "Could not recover reset volume")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}

	inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, vol.Spec.Name)
	if errors.Is(inspectErr, containers.ErrNotFound) {
		var createErr error
		inspected, createErr = createVolume(ctx, r.orchestrator, containers.CreateVolumeOptions{
			Name: vol.Spec.Name, Labels: maps.Clone(record.labels),
		})
		if createErr != nil {
			log.Error(createErr, "Could not recreate volume removed by reset", "VolumeName", vol.Spec.Name)
			return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
		}
	} else if inspectErr != nil {
		log.Error(inspectErr, "Could not inspect reset volume during recovery")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}
	if !reflect.DeepEqual(inspected.Labels, record.labels) {
		log.Error(fmt.Errorf("runtime volume ownership changed"), "Could not recover reset volume")
		return setContainerVolumeState(vol, apiv1.ContainerVolumeStatePending) | additionalReconciliationNeeded
	}
	data.state = apiv1.ContainerVolumeStateReady
	r.volumeData.Update(vol.NamespacedName(), volumeName(vol.Spec.Name), data)
	r.config.VolumeResetRecovery.volumes.Delete(vol.Spec.Name)
	log.Info("Recovered volume removed by reset", "VolumeName", vol.Spec.Name)
	return setContainerVolumeState(vol, apiv1.ContainerVolumeStateReady)
}

func (r *VolumeReconciler) deleteResetVolumeRecovery(ctx context.Context, vol *apiv1.ContainerVolume, record *containerVolumeRecovery, log logr.Logger) objectChange {
	if !record.lock.TryLock() {
		return additionalReconciliationNeeded
	}
	defer record.lock.Unlock()
	if vol.UID != record.volume.UID {
		log.Error(fmt.Errorf("volume resource identity changed"), "Could not delete reset volume recovery")
		return additionalReconciliationNeeded
	}
	if !pointers.TrueValue(vol.Spec.Persistent) {
		inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, vol.Spec.Name)
		if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
			log.Error(inspectErr, "Could not inspect reset volume during deletion")
			return additionalReconciliationNeeded
		}
		if inspected != nil {
			if !reflect.DeepEqual(inspected.Labels, record.labels) {
				log.Error(fmt.Errorf("runtime volume ownership changed"), "Could not delete reset volume")
				return additionalReconciliationNeeded
			}
			if removeErr := removeVolume(ctx, r.orchestrator, vol.Spec.Name); removeErr != nil {
				log.Error(removeErr, "Could not delete reset volume")
				return additionalReconciliationNeeded
			}
		}
	}
	r.config.VolumeResetRecovery.volumes.Delete(vol.Spec.Name)
	r.volumeData.DeleteByNamespacedName(vol.NamespacedName())
	return deleteFinalizer(vol, volumeFinalizer, log)
}
