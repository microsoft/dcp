/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type logicalContainerState int

const (
	logicalContainerStateNamespace logicalContainerState = iota + 1
	logicalContainerStateLifecycle
	logicalContainerStateLookup
	logicalContainerStatePhysical
	logicalContainerStateRuntime
	logicalContainerStatePersistence
	logicalContainerStateDelete
	logicalContainerStateInvalid
)

type logicalContainerOperation struct {
	cancel context.CancelFunc
}

type logicalContainerData struct {
	state      logicalContainerState
	progress   physicalResourceProgress
	reason     apiv2.ConditionReason
	message    string
	retryAfter time.Time
	operation  *logicalContainerOperation
	// Lifecycle key selected for creation or adopted from the inspected container.
	lifecycleKey string
	// Generated runtime name for a session container without spec.containerName.
	generatedContainerName string
	containerID            string
	existingLifecycleKey   string
	replaceExisting        bool
	physicalContainerUID   types.UID
	lease                  *statestore.ResourceLease
	// Lease settlement failures are independent of the original reconciliation failure.
	leaseFailureMessage string
}

func (data *logicalContainerData) Clone() *logicalContainerData {
	cloned := *data
	return &cloned
}

func (data *logicalContainerData) UpdateFrom(other *logicalContainerData) bool {
	if other == nil {
		return false
	}
	left, right := *data, *other
	left.operation, right.operation = nil, nil
	left.lease, right.lease = nil, nil
	if data.operation == other.operation && data.lease == other.lease && reflect.DeepEqual(left, right) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *logicalContainerData) applyTo(container *apiv2.LogicalContainer) (objectChange, AdditionalReconciliationDelay, bool) {
	physicalContainerRef, imageRef := "", ""
	if data.physicalContainerUID != "" {
		physicalContainerRef = "logical-" + string(container.UID)
	}
	if container.Spec.Container != nil {
		if container.Spec.Container.ImageRef != "" {
			imageRef = commonapi.AsNamespacedName(container.Spec.Container.ImageRef, container.Namespace).Name
		} else if physicalContainerRef != "" && data.containerID == "" {
			imageRef = physicalContainerRef
		}
	}
	change := setValue(&container.Status.PhysicalContainerRef, physicalContainerRef)
	change |= setValue(&container.Status.ImageRef, imageRef)
	change |= setValue(&container.Status.LifecycleKey, data.lifecycleKey)
	if data.physicalContainerUID == "" {
		change |= applyLogicalContainerPhysicalDetails(container, &apiv2.PhysicalContainerStatus{})
	}
	state, progress, reason, message := data.state, data.progress, data.reason, data.message
	if data.leaseFailureMessage != "" {
		state, progress = logicalContainerStatePersistence, physicalResourceProgressRetryPending
		reason, message = "", data.leaseFailureMessage
	} else if data.lease != nil && data.operation != nil && data.progress != physicalResourceProgressInProgress {
		// Lease settlement must not overwrite the reconciliation state or its original failure.
		state, progress = logicalContainerStatePersistence, physicalResourceProgressInProgress
		reason, message = "", ""
	}
	projectionChange, delay, valid := logicalContainerProjections.applyWithReason(
		state, progress, reason, message,
		&container.Status.Phase, &container.Status.Conditions, container.Generation,
	)
	return change | projectionChange, delay, valid
}

var logicalContainerProjections = physicalResourceProjectionTable[logicalContainerState, apiv2.LogicalContainerPhase]{
	invalidPhase: apiv2.LogicalContainerPhaseUnknown,
	projections: map[physicalResourceProjectionKey[logicalContainerState]]physicalResourceProjection[apiv2.LogicalContainerPhase]{
		{state: logicalContainerStateNamespace, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotFound,
		},
		{state: logicalContainerStateNamespace, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotReady,
		},
		{state: logicalContainerStateNamespace, progress: physicalResourceProgressTerminating}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceTerminating,
		},
		{state: logicalContainerStateNamespace, progress: physicalResourceProgressNotActive}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceNotActive,
		},
		{state: logicalContainerStateNamespace, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonNamespaceLookupFailed,
			requeue:         true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStateLifecycle, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PreparingLifecycle", message: "Logical container lifecycle preparation is in progress.",
		},
		{state: logicalContainerStateLifecycle, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStateLifecycle, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LifecyclePreparationFailed",
		},
		{state: logicalContainerStateLookup, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "LookingUpContainer", message: "Looking up the existing runtime container.",
		},
		{state: logicalContainerStateLookup, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerLookupFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStateLookup, progress: physicalResourceProgressNotFound}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ExistingContainerNotFound", requeue: true, requeueDelay: MonitoringDelay,
		},
		{state: logicalContainerStatePhysical, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: logicalContainerPreparationPendingReason,
		},
		{state: logicalContainerStatePhysical, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalResourcePreparationFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStatePhysical, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalResourcePreparationFailed",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerNotReady",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressRunning}: {
			phase: apiv2.LogicalContainerPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: "ContainerRunning", message: "The logical container is running.",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressPaused}: {
			phase: apiv2.LogicalContainerPhasePaused, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerPaused", message: "The logical container is paused.",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressExited}: {
			phase: apiv2.LogicalContainerPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: "ContainerExited", message: "The logical container has exited.",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerFailed",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressUnknown}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerStateUnknown",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressMissing}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerMissing",
		},
		{state: logicalContainerStateRuntime, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalContainerLookupFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStatePersistence, progress: physicalResourceProgressInProgress}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "RecordingPersistence", message: "Recording the persistent container and releasing its startup lease.",
		},
		{state: logicalContainerStatePersistence, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PersistenceFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStateDelete, progress: physicalResourceProgressNotReady}: {
			phase: apiv2.LogicalContainerPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: "DeletingPhysicalResources", requeue: true,
		},
		{state: logicalContainerStateDelete, progress: physicalResourceProgressRetryPending}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: "PhysicalResourceDeletionFailed", requeue: true, requeueDelay: LongDelay,
		},
		{state: logicalContainerStateInvalid, progress: physicalResourceProgressFailed}: {
			phase: apiv2.LogicalContainerPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
