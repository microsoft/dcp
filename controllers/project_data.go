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
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/process"
)

type projectState int

func (data *projectData) failMissing(message string) {
	data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonPreparationFailed, message
	if data.prepared {
		data.reason = apiv2.ProjectReasonMissing
	}
}

func (data *projectData) updateLifecycleFromContainer(resource *apiv2.Project, physical *apiv2.PhysicalContainer) {
	if !physical.DeletionTimestamp.IsZero() {
		data.failMissing("The backing PhysicalContainer is being deleted.")
		return
	}
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	message := logicalBackingStatusMessage("PhysicalContainer", physical.Status.Conditions)
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		if condition != nil && condition.Status == metav1.ConditionTrue {
			if resource.Spec.DebugAdapter != nil && resource.Spec.DebugAdapter.GetContainerTCPPort() != 0 && data.adapterAddress == "" {
				data.state = projectStateUnknown
				if !data.prepared {
					data.state = projectStateCreating
				}
				data.reason, data.message = apiv2.ProjectReasonLookupFailed, "Waiting for the container adapter port binding."
				if !data.prepared {
					data.reason = apiv2.ProjectReasonPreparing
				}
				return
			}
			data.prepared = true
			if data.state != projectStateActive {
				data.state = projectStateReady
			}
			data.reason, data.message = "", ""
			return
		}
	case apiv2.PhysicalContainerPhasePaused:
		data.prepared = true
		data.state, data.reason, data.message = projectStatePaused, apiv2.ProjectReasonPaused, message
		return
	case apiv2.PhysicalContainerPhaseExited, apiv2.PhysicalContainerPhaseFailed:
		data.failMissing(message)
		return
	case apiv2.PhysicalContainerPhaseUnknown:
		if condition != nil && condition.Reason == string(apiv2.PhysicalContainerReasonRuntimeContainerMissing) {
			data.failMissing(message)
			return
		}
	case apiv2.PhysicalContainerPhasePending, "":
	default:
		data.state, data.reason, data.message = projectStateFailed, apiv2.PhysicalResourceReasonOperationStateInvalid, fmt.Sprintf("Unrecognized physical container phase %q.", physical.Status.Phase)
		return
	}
	data.state, data.reason, data.message = projectStateUnknown, apiv2.ProjectReasonLookupFailed, message
	if !data.prepared {
		data.state, data.reason = projectStateCreating, apiv2.ProjectReasonPreparing
	}
}

func (data *projectData) requestStop() bool {
	switch data.state {
	case projectStatePending, projectStateCreating, projectStateReady, projectStateActive, projectStatePaused, projectStateUnknown:
		data.state, data.reason, data.message = projectStateStopping, apiv2.ProjectReasonStopping, ""
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
	projectStatePaused
	projectStateStopping
	projectStateStopped
	projectStateFailed
	projectStateUnknown
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
	case projectStatePaused:
		return "Paused"
	case projectStateStopping:
		return "Stopping"
	case projectStateStopped:
		return "Stopped"
	case projectStateFailed:
		return "Failed"
	case projectStateUnknown:
		return "Unknown"
	default:
		return fmt.Sprintf("projectState(%d)", int(state))
	}
}

type projectData struct {
	state              projectState
	reason             apiv2.ConditionReason
	message            string
	physicalUID        types.UID
	physicalName       string
	imageUID           types.UID
	imageName          string
	ports              []logicalPortSelection
	unobservedChildren unobservedLogicalChildren
	effectiveArgs      []string
	effectiveEnv       []commonapi.EnvVar
	inputsPrepared     bool
	// Records successful preparation independently of temporary observation failures.
	prepared               bool
	processes              map[process.ProcessHandle]projectProcessData
	processCleanupsPending int
	processPoll            bool
	adapterAddress         string
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
	return &cloned
}

func (data *projectData) UpdateFrom(other *projectData) bool {
	if other == nil || reflect.DeepEqual(data, other) {
		return false
	}
	*data = *other.Clone()
	return true
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
	arguments, environment := data.effectiveArgs, data.effectiveEnv
	if !slices.Equal(resource.Status.EffectiveArgs, arguments) {
		resource.Status.EffectiveArgs = slices.Clone(arguments)
		change |= statusChanged
	}
	if resource.Spec.Process != nil && !slices.Equal(resource.Status.EffectiveEnv, environment) {
		resource.Status.EffectiveEnv = slices.Clone(environment)
		change |= statusChanged
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
			phase: apiv2.ProjectPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonPreparing,
		},
		projectStateCreating: {
			phase: apiv2.ProjectPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonPreparing,
		},
		projectStateReady: {
			phase: apiv2.ProjectPhaseReady, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonPrepared,
		},
		projectStateActive: {
			phase: apiv2.ProjectPhaseActive, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonDebugSessionsActive,
		},
		projectStatePaused: {
			phase: apiv2.ProjectPhaseReady, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonPaused,
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
			phase: apiv2.ProjectPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonLookupFailed,
			requeue: true, requeueDelay: LongDelay,
		},
	},
}
