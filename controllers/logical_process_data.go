/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"maps"
	"reflect"
	"slices"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/statestore"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type logicalProcessState int

const (
	logicalProcessStatePrepare logicalProcessState = iota + 1
	logicalProcessStatePhysical
	logicalProcessStateCreating
	logicalProcessStateMissing
	logicalProcessStatePrepareFailed
	logicalProcessStatePhysicalFailed
	logicalProcessStateRunning
	logicalProcessStateExited
	logicalProcessStateRuntimeFailed
	logicalProcessStateUnknown
	logicalProcessStateStopping
)

func (state logicalProcessState) String() string {
	switch state {
	case 0:
		return "Unset"
	case logicalProcessStatePrepare:
		return "Prepare"
	case logicalProcessStatePrepareFailed:
		return "PrepareFailed"
	case logicalProcessStatePhysical:
		return "Physical"
	case logicalProcessStatePhysicalFailed:
		return "PhysicalFailed"
	case logicalProcessStateCreating:
		return "Creating"
	case logicalProcessStateRunning:
		return "Running"
	case logicalProcessStateExited:
		return "Exited"
	case logicalProcessStateRuntimeFailed:
		return "RuntimeFailed"
	case logicalProcessStateUnknown:
		return "Unknown"
	case logicalProcessStateStopping:
		return "Stopping"
	case logicalProcessStateMissing:
		return "Missing"
	default:
		return fmt.Sprintf("logicalProcessState(%d)", int(state))
	}
}

func logicalProcessBackingState(physical *apiv2.PhysicalProcess) logicalProcessState {
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	switch physical.Status.Phase {
	case apiv2.PhysicalProcessPhasePending:
		if ready != nil && ready.Reason == string(apiv2.PhysicalProcessReasonStopping) {
			return logicalProcessStateStopping
		}
	case apiv2.PhysicalProcessPhaseRunning:
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return logicalProcessStateRunning
		}
	case apiv2.PhysicalProcessPhaseExited:
		if ready != nil && ready.Reason == string(apiv2.PhysicalProcessReasonRuntimeProcessMissing) {
			return logicalProcessStateMissing
		}
		return logicalProcessStateExited
	case apiv2.PhysicalProcessPhaseFailed:
		return logicalProcessStateRuntimeFailed
	case apiv2.PhysicalProcessPhaseUnknown:
		return logicalProcessStateUnknown
	}
	return logicalProcessStateCreating
}

func logicalPhysicalProcessStartupSettled(physical *apiv2.PhysicalProcess) bool {
	if !physical.DeletionTimestamp.IsZero() {
		return !hasFinalizer(physical, physicalProcessFinalizer)
	}
	ready := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	if ready == nil {
		return false
	}
	switch physical.Status.Phase {
	case apiv2.PhysicalProcessPhaseRunning:
		return ready.Status == metav1.ConditionTrue
	case apiv2.PhysicalProcessPhaseExited, apiv2.PhysicalProcessPhaseFailed:
		return true
	default:
		return physical.Status.Phase == apiv2.PhysicalProcessPhasePending &&
			ready.Reason == string(apiv2.PhysicalResourceReasonNamespaceTerminating)
	}
}

func applyLogicalProcessBackingStatus(resource *apiv2.LogicalProcess, physical *apiv2.PhysicalProcess) (objectChange, logicalProcessState) {
	change := noChange
	status := &physical.Status
	source := physical.GetLogSource()
	if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		resource.Status.RuntimeLogs = source
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.PID, status.PID) {
		resource.Status.PID = status.DeepCopy().PID
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.ExitCode, status.ExitCode) {
		resource.Status.ExitCode = cloneInt32Pointer(status.ExitCode)
		change |= statusChanged
	}
	change |= setTimestamp(&resource.Status.IdentityTimestamp, status.IdentityTimestamp)
	change |= setTimestamp(&resource.Status.FinishedAt, status.FinishedAt)
	return change, logicalProcessBackingState(physical)
}

type logicalProcessData struct {
	state               logicalProcessState
	reason              apiv2.ConditionReason
	message             string
	started             bool
	effectiveArgs       []string
	effectiveEnv        []commonapi.EnvVar
	launchEnv           []commonapi.EnvVar
	inputsPrepared      bool
	ports               []logicalPortSelection
	lifecycleKey        string
	physicalUID         types.UID
	replacementUID      types.UID
	unobservedChildren  unobservedLogicalChildren
	lease               *statestore.ResourceLease
	leaseFailureMessage string
	// Selected process identity and captures, cleared after physical handoff.
	pendingRecord *statestore.PersistentProcessRecord
}

func (data *logicalProcessData) Clone() *logicalProcessData {
	cloned := *data
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
	cloned.effectiveArgs = append([]string(nil), data.effectiveArgs...)
	cloned.effectiveEnv = append([]commonapi.EnvVar(nil), data.effectiveEnv...)
	cloned.launchEnv = slices.Clone(data.launchEnv)
	cloned.ports = append([]logicalPortSelection(nil), data.ports...)
	if data.pendingRecord != nil {
		record := *data.pendingRecord
		cloned.pendingRecord = &record
	}
	return &cloned
}

func (data *logicalProcessData) effectiveSpec(resource *apiv2.LogicalProcess) *apiv2.LogicalProcessSpec {
	spec := resource.Spec.DeepCopy()
	if spec.Process != nil {
		spec.Process.Args = append([]string(nil), data.effectiveArgs...)
		spec.Process.Env = append([]commonapi.EnvVar(nil), data.effectiveEnv...)
	}
	return spec
}

func (data *logicalProcessData) UpdateFrom(other *logicalProcessData) bool {
	if other == nil {
		return false
	}
	left, right := *data, *other
	left.lease, right.lease = nil, nil
	if data.lease == other.lease && reflect.DeepEqual(left, right) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *logicalProcessData) terminal() bool {
	switch data.state {
	case logicalProcessStatePrepareFailed, logicalProcessStatePhysicalFailed, logicalProcessStateRuntimeFailed,
		logicalProcessStateMissing, logicalProcessStateExited:
		return true
	default:
		return false
	}
}

func (data *logicalProcessData) failMissing(message string) {
	data.state, data.reason, data.message = logicalProcessStateRuntimeFailed,
		apiv2.LogicalProcessReasonFailedToStart, message
	if data.started {
		data.reason = apiv2.LogicalProcessReasonMissing
	}
}

func (data *logicalProcessData) updateLifecycleFromPhysicalProcess(physical *apiv2.PhysicalProcess) {
	data.state, data.reason, data.message = logicalProcessBackingState(physical), "", logicalBackingStatusMessage("PhysicalProcess", physical.Status.Conditions)
	switch data.state {
	case logicalProcessStateCreating:
		if data.started {
			data.state = logicalProcessStateUnknown
		}
	case logicalProcessStateRunning:
		data.started = true
	case logicalProcessStateUnknown:
		if !data.started {
			data.state = logicalProcessStateCreating
		}
	case logicalProcessStateStopping:
		if !data.started {
			data.state = logicalProcessStateCreating
		}
	case logicalProcessStateExited:
		data.reason = apiv2.LogicalProcessReasonCompleted
		if physical.Spec.Stop {
			data.reason = apiv2.LogicalProcessReasonStopped
		}
		if physical.Status.ExitCode != nil {
			data.message = fmt.Sprintf("The backing process exited with code %d.", *physical.Status.ExitCode)
		}
	case logicalProcessStateMissing, logicalProcessStateRuntimeFailed:
		data.failMissing(data.message)
	}
}

func (data *logicalProcessData) applyTo(resource *apiv2.LogicalProcess) (objectChange, AdditionalReconciliationDelay, bool) {
	physicalRef := ""
	if data.physicalUID != "" {
		physicalRef = "logical-" + string(resource.UID)
	}
	change := setValue(&resource.Status.PhysicalProcessRef, physicalRef)
	if !slices.Equal(resource.Status.EffectiveArgs, data.effectiveArgs) {
		resource.Status.EffectiveArgs = slices.Clone(data.effectiveArgs)
		change |= statusChanged
	}
	if !slices.Equal(resource.Status.EffectiveEnv, data.launchEnv) {
		resource.Status.EffectiveEnv = slices.Clone(data.launchEnv)
		change |= statusChanged
	}
	var ports []logicalPortData
	lifecycleKey := resource.Spec.LifecycleKey
	if data.physicalUID != "" {
		ports = data.portData(resource)
		lifecycleKey = data.lifecycleKey
	}
	change |= projectLogicalPorts(ports, &resource.Status.Ports)
	change |= setValue(&resource.Status.LifecycleKey, lifecycleKey)
	message := data.message
	if !data.terminal() && data.leaseFailureMessage != "" {
		message = data.leaseFailureMessage
	}
	projectionChange, delay, valid := logicalProcessProjections.applyWithReason(
		data.state, data.reason, message,
		&resource.Status.Phase, &resource.Status.Conditions, resource.Generation,
	)
	return change | projectionChange, delay, valid
}

func (data *logicalProcessData) applyMissingLogSource(resource *apiv2.LogicalProcess) objectChange {
	// Preserve the backing identity so logical-only followers can observe its completion.
	source := &apiv2.LogSource{
		Name: "logical-" + string(resource.UID), UID: data.physicalUID, Complete: true,
	}
	if reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
		return noChange
	}
	resource.Status.RuntimeLogs = source
	return statusChanged
}

var logicalProcessProjections = physicalResourceProjectionTable[logicalProcessState, apiv2.LogicalProcessPhase]{
	invalidPhase: apiv2.LogicalProcessPhaseUnknown,
	projections: map[logicalProcessState]physicalResourceProjection[apiv2.LogicalProcessPhase]{
		logicalProcessStatePrepare: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonStarting,
		},
		logicalProcessStatePrepareFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonFailedToStart,
		},
		logicalProcessStatePhysical: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonStarting, requeue: true,
		},
		logicalProcessStatePhysicalFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonFailedToStart,
		},
		logicalProcessStateCreating: {
			phase: apiv2.LogicalProcessPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonStarting,
		},
		logicalProcessStateRunning: {
			phase: apiv2.LogicalProcessPhaseRunning, conditionStatus: metav1.ConditionTrue,
			conditionReason: apiv2.LogicalProcessReasonRunning,
		},
		logicalProcessStateExited: {
			phase: apiv2.LogicalProcessPhaseExited, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonCompleted,
		},
		logicalProcessStateRuntimeFailed: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonFailedToStart,
		},
		logicalProcessStateUnknown: {
			phase: apiv2.LogicalProcessPhaseUnknown, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonLookupFailed, requeue: true, requeueDelay: LongDelay,
		},
		logicalProcessStateMissing: {
			phase: apiv2.LogicalProcessPhaseFailed, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonMissing,
		},
		logicalProcessStateStopping: {
			phase: apiv2.LogicalProcessPhaseRunning, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalProcessReasonStopping, message: "Waiting for the physical controller to stop the process.", requeue: true,
		},
	},
}
