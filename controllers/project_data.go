/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/commonapi"
)

type projectState int

const (
	projectStatePending projectState = iota + 1
	projectStateCreating
	projectStateReady
	projectStateActive
	projectStateStopping
	projectStateStopped
	projectStateFailed
	projectStateMissing
	projectStateDelete
	projectStateInvalid
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
	case projectStateMissing:
		return "Missing"
	case projectStateDelete:
		return "Delete"
	case projectStateInvalid:
		return "Invalid"
	default:
		return fmt.Sprintf("projectState(%d)", int(state))
	}
}

type projectData struct {
	state            projectState
	reason           apiv2.ConditionReason
	message          string
	reconcileErr     error
	retryAfter       time.Time
	stoppingAt       time.Time
	physicalUID      types.UID
	physicalName     string
	imageUID         types.UID
	imageName        string
	networks         []apiv2.ContainerNetworkConnectionConfig
	networksSelected bool
	ports            []logicalPortData
	processConfig    *apiv2.LogicalProcessConfig
	containerConfig  *apiv2.LogicalContainerConfig
}

func (data *projectData) Clone() *projectData {
	cloned := *data
	cloned.ports = slices.Clone(data.ports)
	cloned.processConfig = data.processConfig.DeepCopy()
	cloned.containerConfig = data.containerConfig.DeepCopy()
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

func (data *projectData) applyTo(resource *apiv2.Project) (objectChange, AdditionalReconciliationDelay, bool) {
	change := projectLogicalPorts(data.ports, &resource.Status.Ports)
	inputsReady := data.processConfig != nil || data.containerConfig != nil
	var arguments []string
	var environment []commonapi.EnvVar
	if data.processConfig != nil {
		arguments, environment = data.processConfig.Args, data.processConfig.Env
	} else if data.containerConfig != nil {
		arguments, environment = data.containerConfig.Command, data.containerConfig.Env
	}
	if !reflect.DeepEqual(resource.Status.EffectiveArgs, arguments) {
		resource.Status.EffectiveArgs = slices.Clone(arguments)
		change |= statusChanged
	}
	if !reflect.DeepEqual(resource.Status.EffectiveEnv, environment) {
		resource.Status.EffectiveEnv = slices.Clone(environment)
		change |= statusChanged
	}
	change |= setValue(&resource.Status.LaunchInputsReady, inputsReady)
	prerequisitesReady := inputsReady && data.reconcileErr == nil &&
		(data.state == projectStateReady || data.state == projectStateActive)
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
	if data.state == projectStateMissing || data.state == projectStateStopped {
		source := &apiv2.LogSource{Name: physicalRef, UID: data.physicalUID, Complete: true}
		if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
			resource.Status.RuntimeLogs = source
			change |= statusChanged
		}
		if data.state == projectStateStopped && resource.Status.StartupLogs != nil {
			startupSource := &apiv2.LogSource{Name: data.imageName, UID: data.imageUID, Complete: true}
			if !reflect.DeepEqual(resource.Status.StartupLogs, startupSource) {
				resource.Status.StartupLogs = startupSource
				change |= statusChanged
			}
		}
	}
	if _, valid := projectProjections.project(data.state); valid && data.reconcileErr != nil {
		change |= setValue(&resource.Status.Phase, apiv2.ProjectPhaseUnknown)
		change |= setCondition(&resource.Status.Conditions, apiv2.ConditionReady, resource.Generation,
			metav1.ConditionFalse, data.reason, data.message)
		return change | additionalReconciliationNeeded, LongDelay, true
	}
	projectionChange, delay, valid := projectProjections.applyWithReason(
		data.state, data.reason, data.message, &resource.Status.Phase, &resource.Status.Conditions, resource.Generation)
	return change | projectionChange, delay, valid
}

var projectProjections = physicalResourceProjectionTable[projectState, apiv2.ProjectPhase]{
	invalidPhase: apiv2.ProjectPhaseFailed,
	projections: map[projectState]physicalResourceProjection[apiv2.ProjectPhase]{
		projectStatePending: {
			phase: apiv2.ProjectPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "PreparingLaunchInputs",
		},
		projectStateCreating: {
			phase: apiv2.ProjectPhasePending, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerPreparationPending",
		},
		projectStateReady: {
			phase: apiv2.ProjectPhaseReady, conditionStatus: metav1.ConditionTrue, conditionReason: "ProjectPrepared",
		},
		projectStateActive: {
			phase: apiv2.ProjectPhaseActive, conditionStatus: metav1.ConditionTrue, conditionReason: "DebugSessionsActive",
		},
		projectStateStopping: {
			phase: apiv2.ProjectPhaseStopping, conditionStatus: metav1.ConditionFalse, conditionReason: "ProjectStopping", requeue: true,
		},
		projectStateStopped: {
			phase: apiv2.ProjectPhaseStopped, conditionStatus: metav1.ConditionFalse, conditionReason: "ProjectStopped",
		},
		projectStateFailed: {
			phase: apiv2.ProjectPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "PreparationFailed",
		},
		projectStateMissing: {
			phase: apiv2.ProjectPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: "ContainerContextMissing",
		},
		projectStateDelete: {
			phase: apiv2.ProjectPhaseStopping, conditionStatus: metav1.ConditionFalse, conditionReason: "DeletingProject", requeue: true,
		},
		projectStateInvalid: {
			phase: apiv2.ProjectPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
		},
	},
}
