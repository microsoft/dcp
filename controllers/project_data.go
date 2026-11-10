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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/process"
)

type projectState int

func projectContainerBackingState(resource *apiv2.Project, physical *apiv2.PhysicalContainer, adapterAddress string) (projectState, apiv2.ConditionReason, string) {
	if !physical.DeletionTimestamp.IsZero() {
		return projectStateFailed, apiv2.ProjectReasonContainerContextTerminating, "The prepared container is being deleted."
	}
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	message := ""
	if condition != nil {
		message = fmt.Sprintf("Container preparation reports %s: %s", condition.Reason, condition.Message)
	}
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		if condition != nil && condition.Status == metav1.ConditionTrue {
			if resource.Spec.DebugAdapter != nil && resource.Spec.DebugAdapter.GetContainerTCPPort() != 0 && adapterAddress == "" {
				return projectStateCreating, apiv2.ProjectReasonAdapterPortPending, "Waiting for the container adapter port binding."
			}
			return projectStateReady, "", ""
		}
	case apiv2.PhysicalContainerPhaseExited, apiv2.PhysicalContainerPhaseFailed:
		return projectStateFailed, apiv2.ProjectReasonPreparationFailed, message
	case apiv2.PhysicalContainerPhaseUnknown, apiv2.PhysicalContainerPhasePaused, apiv2.PhysicalContainerPhasePending, "":
	default:
		return projectStateUnknown, apiv2.PhysicalResourceReasonOperationStateInvalid, fmt.Sprintf("Unrecognized physical container phase %q.", physical.Status.Phase)
	}
	return projectStateCreating, apiv2.ProjectReasonContainerPreparationPending, message
}

func (data *projectData) requestStop() bool {
	switch data.state {
	case projectStatePending, projectStateCreating, projectStateReady, projectStateActive:
		data.state, data.reason, data.message, data.reconcileErr = projectStateStopping, apiv2.ProjectReasonStopping, "", nil
		data.stoppingAt = time.Now()
		return true
	default:
		return false
	}
}

const (
	projectStatePending projectState = iota + 1
	projectStateCreating
	projectStateReady
	projectStateActive
	projectStateStopping
	projectStateStopped
	projectStateFailed
	projectStateUnknown
	projectStateDelete
)

func (state projectState) String() string {
	switch state {
	case 0:
		return "Unset"
	case projectStatePending:
		return "Pending"
	case projectStateCreating:
		return "Creating"
	case projectStateReady:
		return "Ready"
	case projectStateActive:
		return "Active"
	case projectStateStopping:
		return "Stopping"
	case projectStateStopped:
		return "Stopped"
	case projectStateFailed:
		return "Failed"
	case projectStateUnknown:
		return "Unknown"
	case projectStateDelete:
		return "Delete"
	default:
		return fmt.Sprintf("projectState(%d)", int(state))
	}
}

type projectData struct {
	state              projectState
	reason             apiv2.ConditionReason
	message            string
	reconcileErr       error
	reconcileReason    apiv2.ConditionReason
	retryAfter         time.Time
	stoppingAt         time.Time
	physicalUID        types.UID
	physicalName       string
	imageUID           types.UID
	imageName          string
	networks           []apiv2.ContainerNetworkConnectionConfig
	networksSelected   bool
	ports              []logicalPortSelection
	unobservedChildren unobservedLogicalChildren
	effectiveArgs      []string
	effectiveEnv       []commonapi.EnvVar
	inputsPrepared     bool
	processes          map[process.ProcessHandle]projectProcessData
	processCleanup     *projectProcessCleanupOperation
	processPoll        bool
	adapterAddress     string
}

func (data *projectData) Clone() *projectData {
	cloned := *data
	if data.processes != nil {
		cloned.processes = make(map[process.ProcessHandle]projectProcessData, len(data.processes))
		for handle, target := range data.processes {
			cloned.processes[handle] = target
		}
	}
	cloned.ports = slices.Clone(data.ports)
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
	cloned.effectiveArgs = slices.Clone(data.effectiveArgs)
	cloned.effectiveEnv = slices.Clone(data.effectiveEnv)
	cloned.networks = slices.Clone(data.networks)
	for i := range data.networks {
		data.networks[i].DeepCopyInto(&cloned.networks[i])
	}
	return &cloned
}

func (data *projectData) UpdateFrom(other *projectData) bool {
	if other == nil || reflect.DeepEqual(data, other) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *projectData) retry(reason apiv2.ConditionReason, operationErr error) {
	data.reconcileErr, data.reconcileReason = operationErr, reason
	data.retryAfter = time.Now().Add(delayDurations[LongDelay].Duration)
}

func (data *projectData) portData(resource *apiv2.Project) []logicalPortData {
	requests := projectPortRequests(resource)
	var ports []logicalPortData
	for portIndex, selected := range data.ports {
		ports = append(ports, logicalPortForSelection(resource, requests[portIndex], selected))
	}
	return ports
}

func (data *projectData) applyTo(resource *apiv2.Project) (objectChange, AdditionalReconciliationDelay, bool) {
	change := projectLogicalPorts(data.portData(resource), &resource.Status.Ports)
	adapterPort := int32(0)
	if resource.Spec.DebugAdapter != nil {
		adapterPort = resource.Spec.DebugAdapter.GetContainerTCPPort()
	}
	change |= setValue(&resource.Status.DebugAdapterPort, adapterPort)
	change |= setValue(&resource.Status.DebugAdapterAddress, data.adapterAddress)
	change |= projectProcessObservations(resource, data)
	inputsReady := data.inputsPrepared
	arguments, environment := data.effectiveArgs, data.effectiveEnv
	if !slices.Equal(resource.Status.EffectiveArgs, arguments) {
		resource.Status.EffectiveArgs = slices.Clone(arguments)
		change |= statusChanged
	}
	if !slices.Equal(resource.Status.EffectiveEnv, environment) {
		resource.Status.EffectiveEnv = slices.Clone(environment)
		change |= statusChanged
	}
	change |= setValue(&resource.Status.LaunchInputsReady, inputsReady)
	prerequisitesReady := inputsReady && data.reconcileErr == nil &&
		(data.state == projectStateReady || data.state == projectStateActive)
	prerequisitesReady = prerequisitesReady && (adapterPort == 0 || data.adapterAddress != "")
	if resource.Spec.DebugAdapter != nil || resource.Status.Debug != nil {
		if resource.Status.Debug == nil {
			resource.Status.Debug = &apiv2.ProjectDebugStatus{}
			change |= statusChanged
		}
		change |= setValue(&resource.Status.Debug.PrerequisitesReady, prerequisitesReady)
	}
	physicalRef := ""
	if data.physicalUID != "" {
		physicalRef = data.physicalName
	}
	change |= setValue(&resource.Status.PhysicalContainerRef, physicalRef)
	imageRef := ""
	if resource.Spec.Container != nil {
		if resource.Spec.Container.ImageRef != "" {
			imageRef = projectImageRef(resource)
		} else if data.imageUID != "" {
			imageRef = data.imageName
		}
	}
	change |= setValue(&resource.Status.ImageRef, imageRef)
	if data.state == projectStateStopped {
		source := &apiv2.LogSource{Name: physicalRef, UID: data.physicalUID, Complete: true}
		if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
			resource.Status.RuntimeLogs = source
			change |= statusChanged
		}
		if resource.Status.StartupLogs != nil {
			startupSource := &apiv2.LogSource{Name: data.imageName, UID: data.imageUID, Complete: true}
			if !reflect.DeepEqual(resource.Status.StartupLogs, startupSource) {
				resource.Status.StartupLogs = startupSource
				change |= statusChanged
			}
		}
	}
	if _, valid := projectProjections.project(data.state); valid && data.reconcileErr != nil {
		phase := apiv2.ProjectPhaseUnknown
		if data.state == projectStateStopping || data.state == projectStateStopped {
			phase = apiv2.ProjectPhaseStopping
		}
		change |= setValue(&resource.Status.Phase, phase)
		change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation,
			metav1.ConditionFalse, data.reconcileReason, data.reconcileErr.Error())
		return change | additionalReconciliationNeeded, LongDelay, true
	}
	projectionChange, delay, valid := projectProjections.applyWithReason(
		data.state, data.reason, data.message, &resource.Status.Phase, &resource.Status.Conditions, resource.Generation)
	if data.processPoll && projectionChange&additionalReconciliationNeeded == 0 {
		projectionChange |= additionalReconciliationNeeded
		delay = MonitoringDelay
	}
	return change | projectionChange, delay, valid
}

var projectProjections = physicalResourceProjectionTable[projectState, apiv2.ProjectPhase]{
	invalidPhase: apiv2.ProjectPhaseUnknown,
	projections: map[projectState]physicalResourceProjection[apiv2.ProjectPhase]{
		projectStatePending: {
			phase: apiv2.ProjectPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonPreparingLaunchInputs,
		},
		projectStateCreating: {
			phase: apiv2.ProjectPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonContainerPreparationPending,
		},
		projectStateReady: {
			phase: apiv2.ProjectPhaseReady, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonPrepared,
		},
		projectStateActive: {
			phase: apiv2.ProjectPhaseActive, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonDebugSessionsActive,
		},
		projectStateStopping: {
			phase: apiv2.ProjectPhaseStopping, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonStopping, requeue: true,
		},
		projectStateStopped: {
			phase: apiv2.ProjectPhaseStopped, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonStopped,
		},
		projectStateFailed: {
			phase: apiv2.ProjectPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonPreparationFailed,
		},
		projectStateUnknown: {
			phase: apiv2.ProjectPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonContainerContextMissing,
		},
		projectStateDelete: {
			phase: apiv2.ProjectPhaseStopping, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonDeleting,
		},
	},
}
