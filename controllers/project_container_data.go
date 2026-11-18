/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	"reflect"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type projectContainerData struct {
	projectData
	physicalUID    types.UID
	physicalName   string
	imageUID       types.UID
	imageName      string
	adapterAddress string
}

func (data *projectContainerData) Clone() *projectContainerData {
	cloned := *data
	cloned.projectData = data.cloneProjectData()
	return &cloned
}

func (data *projectContainerData) UpdateFrom(other *projectContainerData) bool {
	if other == nil || reflect.DeepEqual(data, other) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *projectContainerData) portData(resource *apiv2.ProjectContainer) []logicalPortData {
	return data.portDataFor(resource, projectContainerPortRequests(resource))
}

func (data *projectContainerData) updateLifecycleFromContainer(resource *apiv2.ProjectContainer, physical *apiv2.PhysicalContainer) {
	if !physical.DeletionTimestamp.IsZero() {
		data.failMissing("The backing PhysicalContainer is being deleted.")
		return
	}
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	message := logicalBackingStatusMessage("PhysicalContainer", physical.Status.Conditions)
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		if condition != nil && condition.Status == metav1.ConditionTrue {
			if !resource.Spec.ShouldStart() {
				data.state, data.reason, data.message = projectStateAwaitingStart, "", ""
				return
			}
			if resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeDirect {
				data.prepared = true
				data.state, data.reason, data.message = projectStateRunning, "", ""
				return
			}
			if projectContainerAdapterPort(resource) != 0 && data.adapterAddress == "" {
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
	case apiv2.PhysicalContainerPhaseExited:
		if resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeDirect &&
			(condition == nil || condition.Reason != string(apiv2.PhysicalContainerReasonRuntimeContainerMissing)) {
			data.prepared = true
			data.state, data.reason, data.message = projectStateExited, apiv2.ProjectReasonCompleted, message
			return
		}
		data.failMissing(message)
		return
	case apiv2.PhysicalContainerPhaseFailed:
		data.failMissing(message)
		return
	case apiv2.PhysicalContainerPhaseUnknown:
		if condition != nil && condition.Reason == string(apiv2.PhysicalContainerReasonRuntimeContainerMissing) {
			data.failMissing(message)
			return
		}
	case apiv2.PhysicalContainerPhasePending, "":
		if condition != nil && condition.Reason == string(apiv2.PhysicalContainerReasonAwaitingStart) && !resource.Spec.ShouldStart() {
			data.state, data.reason, data.message = projectStateAwaitingStart, "", ""
			return
		}
	default:
		data.state, data.reason, data.message = projectStateFailed, apiv2.PhysicalResourceReasonOperationStateInvalid, fmt.Sprintf("Unrecognized physical container phase %q.", physical.Status.Phase)
		return
	}
	data.state, data.reason, data.message = projectStateUnknown, apiv2.ProjectReasonLookupFailed, message
	if !data.prepared {
		data.state, data.reason = projectStateCreating, apiv2.ProjectReasonPreparing
	}
}

func (data *projectContainerData) applyTo(resource *apiv2.ProjectContainer) (objectChange, AdditionalReconciliationDelay, bool) {
	change := setValue(&resource.Status.DebugAdapterPort, projectContainerAdapterPort(resource))
	change |= setValue(&resource.Status.DebugAdapterAddress, data.adapterAddress)
	physicalRef := ""
	if data.physicalUID != "" {
		physicalRef = data.physicalName
	}
	change |= setValue(&resource.Status.PhysicalContainerRef, physicalRef)
	imageRef := ""
	if resource.Spec.Container.ImageRef != "" {
		imageRef = projectContainerImageRef(resource)
	} else if data.imageUID != "" {
		imageRef = data.imageName
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
	commonChange, delay, valid := data.applyCommonTo(resource, projectContainerPortRequests(resource), false)
	return change | commonChange, delay, valid
}
