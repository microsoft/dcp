/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/pointers"
)

type containerVolumeResetTarget struct {
	volume *apiv1.ContainerVolume
	labels map[string]string
}

type containerCreationIdentity struct {
	containerID string
	uid         string
}

func (r *ContainerReconciler) reconcileContainerVolumeResets(ctx context.Context, container *apiv1.Container, log logr.Logger) (objectChange, bool, error) {
	if r.volumeResetController == nil {
		return noChange, false, nil
	}
	operations := &apiv1.ContainerVolumeResetList{}
	if listErr := r.NoCacheClient.List(ctx, operations); listErr != nil {
		return noChange, true, fmt.Errorf("list container reset operations: %w", listErr)
	}
	slices.SortFunc(operations.Items, func(left, right apiv1.ContainerVolumeReset) int {
		return cmp.Or(left.CreationTimestamp.Compare(right.CreationTimestamp.Time), strings.Compare(left.Name, right.Name))
	})
	var selected *apiv1.ContainerVolumeReset
	var active *containerVolumeResetExecution
	for index := range operations.Items {
		operation := &operations.Items[index]
		if operation.Spec.ContainerName != container.Name || operation.Spec.ContainerUID != container.UID ||
			!operation.DeletionTimestamp.IsZero() {
			continue
		}
		execution, found := r.volumeResetController.executions.Load(operation.UID)
		if !found {
			if operation.Status.State != "Succeeded" && operation.Status.State != "Failed" {
				return additionalReconciliationNeeded, true, nil
			}
			continue
		}
		execution.lock.Lock()
		terminal := execution.status.State == "Succeeded" || execution.status.State == "Failed"
		if !terminal && selected != nil {
			execution.status.State = "Failed"
			execution.status.Message = fmt.Sprintf("container already has an active reset operation %q", selected.Name)
			execution.cancel()
			r.volumeResetController.ScheduleReconciliation(operation.NamespacedName())
		} else if !terminal {
			selected, active = operation, execution
		}
		execution.lock.Unlock()
	}
	if selected == nil {
		return noChange, false, nil
	}
	active.lock.Lock()
	result := active.status.DeepCopy()
	if result.State == "Pending" {
		_, data := r.runningContainers.BorrowByNamespacedName(container.NamespacedName())
		if data != nil && (data.containerState == apiv1.ContainerStateBuilding ||
			data.containerState == apiv1.ContainerStateStarting || data.containerState == apiv1.ContainerStateStopping) {
			active.lock.Unlock()
			return r.manageContainer(ctx, container, log) | additionalReconciliationNeeded, true, nil
		}
		result.State = "Running"
		active.status = *result
		active.lock.Unlock()
		r.volumeResetController.ScheduleReconciliation(selected.NamespacedName())
		return additionalReconciliationNeeded, true, nil
	}
	active.executing = true
	active.lock.Unlock()
	if result.State == "Running" {
		resetErr := r.performContainerVolumeReset(active.ctx, container, selected, result, log)
		if resetErr != nil {
			result.State = "Failed"
			result.Message = resetErr.Error()
			log.Error(resetErr, "Could not reset container volumes", "Operation", selected.Name)
		} else {
			result.State = "Succeeded"
		}
	}
	active.lock.Lock()
	result.FinishTimestamp = metav1.NewMicroTime(r.volumeResetController.now().UTC().Truncate(time.Microsecond))
	active.status = *result
	active.executing = false
	active.cancel()
	active.lock.Unlock()
	r.volumeResetController.releaseTarget(selected.UID, selected.Spec.ContainerUID)
	r.volumeResetController.ScheduleReconciliation(selected.NamespacedName())
	change := noChange
	if result.ContainerRemoved {
		change |= statusChanged
		change |= setValue(&container.Status.ContainerID, "")
		change |= setValue(&container.Status.State, apiv1.ContainerStateNotFound)
	}
	return change | additionalReconciliationNeeded, true, nil
}

func (r *ContainerReconciler) performContainerVolumeReset(
	ctx context.Context,
	container *apiv1.Container,
	resetOperation *apiv1.ContainerVolumeReset,
	result *apiv1.ContainerVolumeResetStatus,
	log logr.Logger,
) error {
	if container.Spec.EffectiveMode() != apiv1.ContainerModeSession && container.Spec.EffectiveMode() != apiv1.ContainerModePersistent {
		return fmt.Errorf("only session and persistent containers can reset volumes")
	}
	_, data := r.runningContainers.BorrowByNamespacedName(container.NamespacedName())
	if data == nil || !data.hasValidContainerID() {
		return fmt.Errorf("container has no physical container to reset")
	}
	target, inspectErr := inspectContainer(ctx, r.orchestrator, string(data.containerID))
	if inspectErr != nil {
		return fmt.Errorf("inspect reset target: %w", inspectErr)
	}

	volumeNames := map[string]bool{}
	for _, mount := range container.Spec.VolumeMounts {
		if mount.Type == apiv1.NamedVolumeMount {
			volumeNames[mount.Source] = true
		}
	}
	for _, mount := range target.Mounts {
		if mount.Type == containers.NamedVolumeMount {
			volumeNames[mount.Source] = true
		}
	}
	if len(volumeNames) == 0 {
		return fmt.Errorf("container has no named volumes to reset; bind mounts are not reset")
	}

	volumeList := &apiv1.ContainerVolumeList{}
	if listErr := r.NoCacheClient.List(ctx, volumeList); listErr != nil {
		return fmt.Errorf("list container volumes: %w", listErr)
	}
	resetTargets := make([]containerVolumeResetTarget, 0, len(volumeNames))
	for _, volumeName := range slices.Sorted(maps.Keys(volumeNames)) {
		var volume *apiv1.ContainerVolume
		for index := range volumeList.Items {
			candidate := &volumeList.Items[index]
			if candidate.Spec.Name != volumeName {
				continue
			}
			if volume != nil {
				return fmt.Errorf("volume %q has multiple ContainerVolume resources", volumeName)
			}
			volume = candidate
		}
		if volume == nil || !volume.DeletionTimestamp.IsZero() || volume.Status.State != apiv1.ContainerVolumeStateReady {
			return fmt.Errorf("volume %q is not a ready DCP ContainerVolume", volumeName)
		}
		resetTargets = append(resetTargets, containerVolumeResetTarget{volume: volume})
	}

	// Hold the same leases used by volume creation and persistent-container lifecycle operations.
	// Volume removal is deliberately non-forced, so a racing external mount cannot be destroyed.
	operation := func(operationCtx context.Context) error {
		if ownershipErr := r.verifyResetContainerOwnership(operationCtx, container, target); ownershipErr != nil {
			return ownershipErr
		}
		if consumerErr := r.findResetVolumeConsumers(operationCtx, container, target.Id, volumeNames, result); consumerErr != nil {
			return consumerErr
		}
		for index := range resetTargets {
			resetTarget := &resetTargets[index]
			currentVolumeObject := &apiv1.ContainerVolume{}
			if volumeGetErr := r.NoCacheClient.Get(operationCtx, ctrl_client.ObjectKeyFromObject(resetTarget.volume), currentVolumeObject); volumeGetErr != nil {
				return fmt.Errorf("revalidate volume %q resource: %w", resetTarget.volume.Spec.Name, volumeGetErr)
			}
			if currentVolumeObject.UID != resetTarget.volume.UID || !currentVolumeObject.DeletionTimestamp.IsZero() {
				return fmt.Errorf("volume %q resource was replaced or deleted", resetTarget.volume.Spec.Name)
			}
			inspectedVolume, volumeInspectErr := inspectContainerVolume(operationCtx, r.orchestrator, resetTarget.volume.Spec.Name)
			if volumeInspectErr != nil {
				return fmt.Errorf("inspect volume %q: %w", resetTarget.volume.Spec.Name, volumeInspectErr)
			}
			if ownershipErr := r.verifyResetVolumeOwnership(operationCtx, resetTarget.volume, inspectedVolume); ownershipErr != nil {
				return ownershipErr
			}
			resetTarget.labels = maps.Clone(inspectedVolume.Labels)
		}
		current := &apiv1.Container{}
		if getErr := r.NoCacheClient.Get(operationCtx, ctrl_client.ObjectKeyFromObject(container), current); getErr != nil {
			return fmt.Errorf("revalidate reset request: %w", getErr)
		}
		currentOperation := &apiv1.ContainerVolumeReset{}
		if getOperationErr := r.NoCacheClient.Get(operationCtx, ctrl_client.ObjectKeyFromObject(resetOperation), currentOperation); getOperationErr != nil {
			return fmt.Errorf("revalidate reset operation: %w", getOperationErr)
		}
		if currentOperation.UID != resetOperation.UID || !currentOperation.DeletionTimestamp.IsZero() ||
			current.UID != resetOperation.Spec.ContainerUID || !current.DeletionTimestamp.IsZero() ||
			!slices.Equal(current.Spec.VolumeMounts, container.Spec.VolumeMounts) {
			return fmt.Errorf("reset request was cancelled or its Container was deleted")
		}
		if contextErr := operationCtx.Err(); contextErr != nil {
			return contextErr
		}
		recoveries := make([]*containerVolumeRecovery, 0, len(resetTargets))
		defer func() {
			for _, recovery := range recoveries {
				r.config.VolumeResetRecovery.finish(recovery)
			}
		}()
		for _, resetTarget := range resetTargets {
			recovery, recoveryErr := r.config.VolumeResetRecovery.begin(resetTarget.volume, resetTarget.labels)
			if recoveryErr != nil {
				return recoveryErr
			}
			recoveries = append(recoveries, recovery)
		}
		if _, stopErr := r.stopContainerIfNecessary(operationCtx, data.containerID, target, log); stopErr != nil {
			return fmt.Errorf("stop reset target: %w", stopErr)
		}
		if removeErr := removeContainer(operationCtx, r.orchestrator, target.Id); removeErr != nil {
			return fmt.Errorf("remove reset target: %w", removeErr)
		}
		result.ContainerRemoved = true
		r.createdPersistentContainers.Delete(container.GetLeaseKey())
		r.cleanupDcpContainerResources(operationCtx, container, log)
		r.removeContainerNetworkConnections(operationCtx, container, log)
		data.closeTerminalResources(operationCtx, r.config.ProcessExecutor, log)
		data.deleteStartupLogFiles(log)
		removedData := newRunningContainerData(container)
		removedData.containerState = apiv1.ContainerStateNotFound
		r.runningContainers.Store(container.NamespacedName(), removedData.containerID, removedData)
		container.Status = apiv1.ContainerStatus{State: apiv1.ContainerStateNotFound, ContainerName: container.Spec.ContainerName}

		if container.Spec.EffectiveMode() == apiv1.ContainerModePersistent && r.config.WorkloadID != "" {
			if deleteRecordErr := r.config.StateStore.DeletePersistentContainer(operationCtx, container.GetLeaseKey()); deleteRecordErr != nil {
				return fmt.Errorf("remove persistent container record: %w", deleteRecordErr)
			}
		}

		for index, resetTarget := range resetTargets {
			volumeName := resetTarget.volume.Spec.Name
			// Recheck identity immediately before removal, rather than deleting a replaced/adopted volume.
			currentVolume, revalidateErr := inspectContainerVolume(operationCtx, r.orchestrator, volumeName)
			if revalidateErr != nil {
				return fmt.Errorf("revalidate volume %q: %w", volumeName, revalidateErr)
			}
			if !reflect.DeepEqual(currentVolume.Labels, resetTarget.labels) {
				return fmt.Errorf("volume %q ownership changed during reset", volumeName)
			}
			recoveries[index].repairRequired = true
			if volumeRemoveErr := removeVolume(operationCtx, r.orchestrator, volumeName); volumeRemoveErr != nil {
				return fmt.Errorf("remove volume %q: %w", volumeName, volumeRemoveErr)
			}
			recreated, createErr := createVolume(operationCtx, r.orchestrator, containers.CreateVolumeOptions{
				Name: volumeName, Labels: resetTarget.labels,
			})
			if createErr != nil {
				return fmt.Errorf("recreate volume %q (the original volume was removed): %w", volumeName, createErr)
			}
			if !reflect.DeepEqual(recreated.Labels, resetTarget.labels) {
				return fmt.Errorf("volume %q was replaced concurrently after removal", volumeName)
			}
			result.Volumes = append(result.Volumes, volumeName)
			recoveries[index].repairRequired = false
		}
		return operationCtx.Err()
	}

	for index := len(resetTargets) - 1; index >= 0; index-- {
		volume := resetTargets[index].volume
		if !pointers.TrueValue(volume.Spec.Persistent) {
			continue
		}
		if r.config.StateStore == nil {
			return fmt.Errorf("state store is required to reset persistent volumes")
		}
		nextOperation := operation
		operation = func(leaseCtx context.Context) error {
			return r.config.StateStore.WithResourceLease(leaseCtx, volume, r.config.ResourceLeaseOwner,
				resourceLeaseRevalidationInterval, func(heldCtx context.Context, _ *statestore.ResourceLease) error {
					return nextOperation(heldCtx)
				})
		}
	}
	if container.Spec.EffectiveMode() == apiv1.ContainerModePersistent {
		if r.config.StateStore == nil {
			return fmt.Errorf("state store is required to reset a persistent container")
		}
		return r.config.StateStore.WithResourceLease(ctx, container, r.config.ResourceLeaseOwner,
			resourceLeaseRevalidationInterval, func(heldCtx context.Context, _ *statestore.ResourceLease) error {
				return operation(heldCtx)
			})
	}
	return operation(ctx)
}

func (r *ContainerReconciler) verifyResetContainerOwnership(ctx context.Context, container *apiv1.Container, target *containers.InspectedContainer) error {
	if container.Spec.EffectiveMode() == apiv1.ContainerModePersistent && r.config.StateStore != nil {
		record, recordErr := r.config.StateStore.GetPersistentContainer(ctx, container.GetLeaseKey())
		if recordErr != nil && !errors.Is(recordErr, statestore.ErrPersistentContainerNotFound) {
			return fmt.Errorf("verify container ownership: %w", recordErr)
		}
		if recordErr == nil {
			if record.ContainerID == target.Id && record.RuntimeName == r.orchestrator.Name() &&
				record.WorkloadID == r.config.WorkloadID && target.Labels[uidLabel] != "" &&
				target.Labels[PersistentLabel] == "true" {
				return nil
			}
			return fmt.Errorf("physical container %q is not owned by this workload", target.Id)
		}
	}
	if container.UID != "" && target.Labels[uidLabel] == string(container.UID) {
		return nil
	}
	if container.Spec.EffectiveMode() == apiv1.ContainerModePersistent {
		identity, created := r.createdPersistentContainers.Load(container.GetLeaseKey())
		if created && identity.containerID == target.Id && identity.uid != "" &&
			target.Labels[uidLabel] == identity.uid && target.Labels[PersistentLabel] == "true" {
			return nil
		}
	}
	return fmt.Errorf("physical container %q is not owned by this workload", target.Id)
}

func (r *ContainerReconciler) verifyResetVolumeOwnership(ctx context.Context, volume *apiv1.ContainerVolume, inspected *containers.InspectedVolume) error {
	if pointers.TrueValue(volume.Spec.Persistent) && r.config.StateStore != nil {
		record, recordErr := r.config.StateStore.GetPersistentVolume(ctx, volume.GetLeaseKey())
		if recordErr != nil && !errors.Is(recordErr, statestore.ErrPersistentVolumeNotFound) {
			return fmt.Errorf("verify volume %q ownership: %w", volume.Spec.Name, recordErr)
		}
		if recordErr == nil {
			if record.WorkloadID == r.config.WorkloadID && record.RuntimeName == r.orchestrator.Name() &&
				record.VolumeName == volume.Spec.Name && persistentVolumeOwnershipMatches(inspected, record.OwnershipToken) {
				return nil
			}
			return fmt.Errorf("volume %q is not owned by this workload", volume.Spec.Name)
		}
	}
	if volume.UID != "" && inspected.Labels[uidLabel] == string(volume.UID) {
		return nil
	}
	return fmt.Errorf("volume %q is external, adopted, or not owned by this workload", volume.Spec.Name)
}

func (r *ContainerReconciler) findResetVolumeConsumers(
	ctx context.Context,
	container *apiv1.Container,
	targetID string,
	volumeNames map[string]bool,
	result *apiv1.ContainerVolumeResetStatus,
) error {
	apiContainers := &apiv1.ContainerList{}
	if listErr := r.NoCacheClient.List(ctx, apiContainers); listErr != nil {
		return fmt.Errorf("list API volume consumers: %w", listErr)
	}
	for _, candidate := range apiContainers.Items {
		if candidate.UID == container.UID {
			continue
		}
		for _, mount := range candidate.Spec.VolumeMounts {
			if mount.Type == apiv1.NamedVolumeMount && volumeNames[mount.Source] {
				result.Consumers = append(result.Consumers, apiv1.ContainerVolumeResetConsumer{
					VolumeName: mount.Source, ContainerName: candidate.Name,
				})
			}
		}
	}
	runtimeContainers, runtimeListErr := r.orchestrator.ListContainers(ctx, containers.ListContainersOptions{All: true})
	if runtimeListErr != nil {
		return fmt.Errorf("list runtime volume consumers: %w", runtimeListErr)
	}
	for _, candidate := range runtimeContainers {
		if candidate.Id == targetID {
			continue
		}
		inspected, inspectErr := inspectContainer(ctx, r.orchestrator, candidate.Id)
		if inspectErr != nil {
			return fmt.Errorf("inspect possible volume consumer %q: %w", candidate.Id, inspectErr)
		}
		for _, mount := range inspected.Mounts {
			if mount.Type == containers.NamedVolumeMount && volumeNames[mount.Source] {
				result.Consumers = append(result.Consumers, apiv1.ContainerVolumeResetConsumer{
					VolumeName: mount.Source, ContainerName: inspected.Name, ContainerID: inspected.Id,
				})
			}
		}
	}
	slices.SortFunc(result.Consumers, func(left, right apiv1.ContainerVolumeResetConsumer) int {
		return cmp.Or(strings.Compare(left.VolumeName, right.VolumeName),
			strings.Compare(left.ContainerName, right.ContainerName),
			strings.Compare(left.ContainerID, right.ContainerID))
	})
	result.Consumers = slices.Compact(result.Consumers)
	if len(result.Consumers) != 0 {
		consumerNames := make([]string, 0, len(result.Consumers))
		for _, consumer := range result.Consumers {
			consumerNames = append(consumerNames, fmt.Sprintf("%s (volume %s)", consumer.ContainerName, consumer.VolumeName))
		}
		return fmt.Errorf("named volumes have other consumers: %s; reset refused before stopping the container", strings.Join(consumerNames, ", "))
	}
	return nil
}
