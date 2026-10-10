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
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/pointers"
)

var errContainerVolumesRecovering = errors.New("container volume generation is not ready")

// ContainerVolumeGenerations connects Container startup to the Volume controller's authoritative selection.
// Share one instance between the Container and Volume reconcilers.
type ContainerVolumeGenerations struct {
	controller *VolumeReconciler
}

func physicalVolumeName(logicalName string, generation int64) string {
	if generation == 0 {
		return logicalName
	}
	return fmt.Sprintf("%s-dcp-%d", logicalName, generation)
}

func generationFromPhysicalName(logicalName, physicalName string) (int64, error) {
	if physicalName == logicalName {
		return 0, nil
	}
	prefix := logicalName + "-dcp-"
	if !strings.HasPrefix(physicalName, prefix) {
		return 0, fmt.Errorf("volume %q is not a generation of %q", physicalName, logicalName)
	}
	generation, parseErr := strconv.ParseInt(strings.TrimPrefix(physicalName, prefix), 10, 64)
	if parseErr != nil || generation <= 0 || physicalVolumeName(logicalName, generation) != physicalName {
		return 0, fmt.Errorf("invalid physical volume generation %q", physicalName)
	}
	return generation, nil
}

func (r *ContainerReconciler) resolveVolumeMounts(ctx context.Context, spec *apiv1.ContainerSpec) ([]apiv1.VolumeMount, error) {
	mounts := append([]apiv1.VolumeMount(nil), spec.VolumeMounts...)
	if !slicesHaveNamedVolumes(mounts) {
		return mounts, nil
	}
	volumes := &apiv1.ContainerVolumeList{}
	if listErr := r.NoCacheClient.List(ctx, volumes); listErr != nil {
		return nil, fmt.Errorf("list container volume dependencies: %w", listErr)
	}
	for index := range mounts {
		mount := &mounts[index]
		if mount.Type != apiv1.NamedVolumeMount {
			continue
		}
		managed := false
		logicalName := mount.Source
		for _, volume := range volumes.Items {
			if volume.Spec.Name != logicalName {
				continue
			}
			if managed {
				return nil, fmt.Errorf("volume %q has multiple ContainerVolume resources", logicalName)
			}
			managed = true
			if !volume.DeletionTimestamp.IsZero() || r.config.VolumeGenerations == nil || r.config.VolumeGenerations.controller == nil {
				return nil, fmt.Errorf("%w: %s", errContainerVolumesRecovering, mount.Source)
			}
			_, data := r.config.VolumeGenerations.controller.volumeData.BorrowByNamespacedName(volume.NamespacedName())
			if data == nil || data.state != apiv1.ContainerVolumeStateReady || data.physicalName == "" || data.generation < volume.Spec.Generation {
				return nil, fmt.Errorf("%w: %s", errContainerVolumesRecovering, mount.Source)
			}
			selected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, data.physicalName)
			if errors.Is(inspectErr, containers.ErrNotFound) {
				r.config.VolumeGenerations.controller.ScheduleReconciliation(volume.NamespacedName())
				return nil, fmt.Errorf("%w: %s", errContainerVolumesRecovering, logicalName)
			}
			if inspectErr != nil {
				return nil, fmt.Errorf("verify selected volume %q: %w", logicalName, inspectErr)
			}
			if !maps.Equal(data.labels, selected.Labels) {
				return nil, fmt.Errorf("selected volume %q ownership changed", logicalName)
			}
			mount.Source = data.physicalName
		}
		if !managed && r.config.StateStore != nil && r.config.WorkloadID != "" {
			key := (&apiv1.ContainerVolume{Spec: apiv1.ContainerVolumeSpec{Name: mount.Source}}).GetLeaseKey()
			record, recordErr := r.config.StateStore.GetPersistentVolume(ctx, key)
			if recordErr != nil && !errors.Is(recordErr, statestore.ErrPersistentVolumeNotFound) {
				return nil, recordErr
			}
			if recordErr == nil && record.WorkloadID == r.config.WorkloadID {
				inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, record.VolumeName)
				if errors.Is(inspectErr, containers.ErrNotFound) {
					return nil, fmt.Errorf("%w: %s", errContainerVolumesRecovering, mount.Source)
				}
				if inspectErr != nil {
					return nil, inspectErr
				}
				if record.RuntimeName != r.orchestrator.Name() || !persistentVolumeOwnershipMatches(inspected, record.OwnershipToken) {
					return nil, fmt.Errorf("managed volume %q ownership does not match its persistent record", mount.Source)
				}
				mount.Source = record.VolumeName
			}
		}
	}
	return mounts, nil
}

func slicesHaveNamedVolumes(mounts []apiv1.VolumeMount) bool {
	for _, mount := range mounts {
		if mount.Type == apiv1.NamedVolumeMount {
			return true
		}
	}
	return false
}

// selectVolumeGeneration preserves the committed persistent head instead of falling back to old data.
func (r *VolumeReconciler) selectVolumeGeneration(ctx context.Context, vol *apiv1.ContainerVolume, data *containerVolumeData) error {
	if data.physicalName != "" && vol.Spec.Generation > data.generation {
		originalUID, created := r.createdVolumes.Load(vol.Spec.Name)
		owned := data.labels[uidLabel] == string(vol.UID) || (created && originalUID != "" && data.labels[uidLabel] == originalUID)
		if pointers.TrueValue(vol.Spec.Persistent) && r.config.WorkloadID != "" {
			record, recordErr := r.config.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
			if recordErr != nil {
				return recordErr
			}
			owned = record.WorkloadID == r.config.WorkloadID && record.RuntimeName == r.orchestrator.Name() &&
				record.OwnershipToken != "" && record.OwnershipToken == data.labels[containers.VolumeOwnershipTokenLabel]
		}
		if !owned {
			return fmt.Errorf("volume %q is adopted or not owned; cannot advance generation", vol.Spec.Name)
		}
	}
	generation := max(vol.Spec.Generation, data.generation)
	physicalName := physicalVolumeName(vol.Spec.Name, generation)
	if pointers.TrueValue(vol.Spec.Persistent) && r.config.WorkloadID != "" {
		record, recordErr := r.config.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
		if recordErr != nil && !errors.Is(recordErr, statestore.ErrPersistentVolumeNotFound) {
			return recordErr
		}
		if recordErr == nil {
			if record.WorkloadID != r.config.WorkloadID || record.RuntimeName != r.orchestrator.Name() {
				if generation != 0 {
					return fmt.Errorf("volume %q is owned by another workload or runtime", vol.Spec.Name)
				}
				existing, inspectExistingErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, physicalName)
				if inspectExistingErr != nil || existing == nil {
					return fmt.Errorf("volume %q belongs to another workload or runtime; cannot create storage", vol.Spec.Name)
				}
			} else {
				recordGeneration, generationErr := generationFromPhysicalName(vol.Spec.Name, record.VolumeName)
				if generationErr != nil {
					return generationErr
				}
				generation = max(generation, recordGeneration)
				physicalName = physicalVolumeName(vol.Spec.Name, generation)
				if data.labels == nil {
					data.labels = map[string]string{uidLabel: string(vol.UID), containers.VolumeOwnershipTokenLabel: record.OwnershipToken}
				}
			}
		}
	} else if data.physicalName == "" && pointers.TrueValue(vol.Spec.Persistent) {
		// Without a workload record, labels select existing storage but do not authorize reset.
		listed, listErr := r.orchestrator.ListVolumes(ctx, containers.ListVolumesOptions{
			Filters: containers.ListVolumesFilters{LabelFilters: []containers.LabelFilter{{Key: containers.VolumeLogicalNameLabel, Value: vol.Spec.Name}}},
		})
		if listErr != nil {
			return fmt.Errorf("discover volume generations: %w", listErr)
		}
		for _, candidate := range listed {
			inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, candidate.Name)
			if inspectErr != nil {
				return inspectErr
			}
			candidateGeneration, generationErr := generationFromPhysicalName(vol.Spec.Name, candidate.Name)
			if generationErr != nil || inspected.Labels[containers.VolumeLogicalNameLabel] != vol.Spec.Name ||
				inspected.Labels[containers.VolumeGenerationLabel] != strconv.FormatInt(candidateGeneration, 10) {
				return fmt.Errorf("invalid generation identity for volume %q", candidate.Name)
			}
			if candidateGeneration > generation {
				generation, physicalName = candidateGeneration, candidate.Name
			}
		}
	}
	inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, physicalName)
	if inspectErr != nil && !errors.Is(inspectErr, containers.ErrNotFound) {
		return inspectErr
	}
	if inspectErr == nil {
		if data.physicalName == physicalName && !maps.Equal(data.labels, inspected.Labels) {
			return fmt.Errorf("selected volume %q ownership changed", physicalName)
		}
		if data.labels != nil && data.labels[containers.VolumeOwnershipTokenLabel] != "" &&
			!persistentVolumeOwnershipMatches(inspected, data.labels[containers.VolumeOwnershipTokenLabel]) {
			if generation > 0 {
				return fmt.Errorf("volume %q ownership conflicts with its persistent record", physicalName)
			}
			if discardErr := r.discardPendingPersistentVolumeRecord(ctx, vol, data.labels[containers.VolumeOwnershipTokenLabel]); discardErr != nil {
				return discardErr
			}
		}
		if generation > 0 && (inspected.Labels[containers.VolumeLogicalNameLabel] != vol.Spec.Name ||
			inspected.Labels[containers.VolumeGenerationLabel] != strconv.FormatInt(generation, 10)) {
			return fmt.Errorf("volume %q has conflicting generation labels", physicalName)
		}
		if generation > data.generation && data.physicalName != "" && data.labels[containers.VolumeOwnershipTokenLabel] == "" &&
			inspected.Labels[uidLabel] != data.labels[uidLabel] {
			return fmt.Errorf("volume generation %q has conflicting ownership", physicalName)
		}
		data.labels = maps.Clone(inspected.Labels)
	} else {
		if data.labels == nil {
			token, prepareErr := r.preparePersistentVolumeRecord(ctx, vol)
			if prepareErr != nil {
				return prepareErr
			}
			data.labels = map[string]string{uidLabel: string(vol.UID)}
			if token != "" {
				data.labels[containers.VolumeOwnershipTokenLabel] = token
			}
		}
		data.labels[containers.VolumeLogicalNameLabel] = vol.Spec.Name
		data.labels[containers.VolumeGenerationLabel] = strconv.FormatInt(generation, 10)
		if pointers.TrueValue(vol.Spec.Persistent) && r.config.WorkloadID != "" {
			persistErr := r.config.StateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
				ResourceKey: vol.GetLeaseKey(), VolumeName: physicalName, RuntimeName: r.orchestrator.Name(),
				WorkloadID: r.config.WorkloadID, OwnershipToken: data.labels[containers.VolumeOwnershipTokenLabel],
			})
			if persistErr != nil {
				return persistErr
			}
		}
		created, createErr := createVolume(ctx, r.orchestrator, containers.CreateVolumeOptions{Name: physicalName, Labels: maps.Clone(data.labels)})
		if errors.Is(createErr, containers.ErrAlreadyExists) {
			created, createErr = inspectContainerVolumeIfExists(ctx, r.orchestrator, physicalName)
		}
		if createErr != nil {
			return fmt.Errorf("create fresh volume generation %q: %w", physicalName, createErr)
		}
		if !maps.Equal(created.Labels, data.labels) {
			if generation > 0 {
				return fmt.Errorf("volume generation %q was created with conflicting ownership", physicalName)
			}
			if token := data.labels[containers.VolumeOwnershipTokenLabel]; token != "" {
				if discardErr := r.discardPendingPersistentVolumeRecord(ctx, vol, token); discardErr != nil {
					return discardErr
				}
			}
			data.labels = maps.Clone(created.Labels)
		} else {
			r.createdVolumes.Store(vol.Spec.Name, data.labels[uidLabel])
		}
	}
	if pointers.TrueValue(vol.Spec.Persistent) && r.config.WorkloadID != "" && generation > 0 {
		token := data.labels[containers.VolumeOwnershipTokenLabel]
		if token == "" {
			return fmt.Errorf("volume generation %q has no workload ownership token", physicalName)
		}
		if persistErr := r.config.StateStore.UpsertPersistentVolume(ctx, statestore.PersistentVolumeRecord{
			ResourceKey: vol.GetLeaseKey(), VolumeName: physicalName, RuntimeName: r.orchestrator.Name(),
			WorkloadID: r.config.WorkloadID, OwnershipToken: token,
		}); persistErr != nil {
			return fmt.Errorf("commit selected volume generation %q: %w", physicalName, persistErr)
		}
	}
	data.physicalName, data.generation = physicalName, generation
	data.state, data.message = apiv1.ContainerVolumeStateReady, ""
	return nil
}

func (r *VolumeReconciler) retireVolumeGenerations(ctx context.Context, vol *apiv1.ContainerVolume, data *containerVolumeData, log logr.Logger) {
	originalUID, created := r.createdVolumes.Load(vol.Spec.Name)
	ownedHead := data.labels[uidLabel] == string(vol.UID) ||
		(created && originalUID != "" && data.labels[uidLabel] == originalUID)
	if pointers.TrueValue(vol.Spec.Persistent) && r.config.WorkloadID != "" {
		record, recordErr := r.config.StateStore.GetPersistentVolume(ctx, vol.GetLeaseKey())
		if errors.Is(recordErr, statestore.ErrPersistentVolumeNotFound) {
			return
		}
		if recordErr != nil {
			log.Error(recordErr, "Could not verify volume generation retirement ownership")
			return
		}
		ownedHead = record.WorkloadID == r.config.WorkloadID && record.RuntimeName == r.orchestrator.Name() &&
			record.VolumeName == data.physicalName && record.OwnershipToken != "" &&
			record.OwnershipToken == data.labels[containers.VolumeOwnershipTokenLabel]
	}
	if !ownedHead {
		return
	}
	listed, listErr := r.orchestrator.ListVolumes(ctx, containers.ListVolumesOptions{
		Filters: containers.ListVolumesFilters{LabelFilters: []containers.LabelFilter{{Key: containers.VolumeLogicalNameLabel, Value: vol.Spec.Name}}},
	})
	if listErr != nil {
		log.Error(listErr, "Could not list retired volume generations")
		return
	}
	if data.generation > 0 {
		listed = append(listed, containers.ListedVolume{Name: vol.Spec.Name})
	}
	for _, candidate := range listed {
		generation, generationErr := generationFromPhysicalName(vol.Spec.Name, candidate.Name)
		if generationErr != nil || generation >= data.generation {
			continue
		}
		inspected, inspectErr := inspectContainerVolumeIfExists(ctx, r.orchestrator, candidate.Name)
		if errors.Is(inspectErr, containers.ErrNotFound) {
			continue
		}
		if inspectErr != nil {
			log.Error(inspectErr, "Could not inspect retired volume generation")
			continue
		}
		token := data.labels[containers.VolumeOwnershipTokenLabel]
		owned := token != "" && persistentVolumeOwnershipMatches(inspected, token)
		if token == "" {
			owned = data.labels[uidLabel] != "" && inspected.Labels[uidLabel] == data.labels[uidLabel]
		}
		legacyOriginal := generation == 0 && inspected.Labels[containers.VolumeLogicalNameLabel] == "" && inspected.Labels[containers.VolumeGenerationLabel] == ""
		if !owned || (!legacyOriginal && (inspected.Labels[containers.VolumeLogicalNameLabel] != vol.Spec.Name ||
			inspected.Labels[containers.VolumeGenerationLabel] != strconv.FormatInt(generation, 10))) {
			continue
		}
		if removeErr := removeVolume(ctx, r.orchestrator, candidate.Name); removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
			if errors.Is(removeErr, containers.ErrObjectInUse) {
				log.V(1).Info("Retired volume generation remains referenced", "Volume", candidate.Name)
			} else {
				log.Error(removeErr, "Could not remove retired volume generation", "Volume", candidate.Name)
			}
		}
	}
}
