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
	condition := apimeta.FindStatusCondition(physical.Status.Conditions, string(apiv2.ConditionReady))
	message := physicalStatusMessage("PhysicalContainer", physical.Status.Conditions)
	switch physical.Status.Phase {
	case apiv2.PhysicalContainerPhaseRunning:
		if condition != nil && condition.Status == metav1.ConditionTrue {
			if resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeDirect {
				data.prepared = true
				data.state, data.reason, data.message = projectStateRunning, "", ""
				return
			}
			data.markAdapterReady()
			return
		}
	case apiv2.PhysicalContainerPhasePaused:
		data.prepared = true
		data.state, data.reason, data.message = projectStatePaused, apiv2.ProjectReasonPaused, message
		return
	case apiv2.PhysicalContainerPhaseExited:
		// An adapter-mode container exits with its primary adapter, which ends the Project run.
		if condition == nil || condition.Reason != string(apiv2.PhysicalContainerReasonRuntimeContainerMissing) {
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
		// The debug runtime starts an adapter-mode container when the first debug session is requested. CreatedAt is
		// observed by the same inspection as the container environment, which the Ready Project publishes.
		if condition != nil && condition.Reason == string(apiv2.PhysicalContainerReasonAwaitingStart) &&
			resource.Spec.EffectiveLaunchMode() == apiv2.ProjectLaunchModeAdapter && physical.Status.ContainerID != "" &&
			!physical.Status.CreatedAt.IsZero() {
			data.markAdapterReady()
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

func (data *projectContainerData) markAdapterReady() {
	data.prepared = true
	if data.state != projectStateActive {
		data.state = projectStateReady
	}
	data.reason, data.message = "", ""
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
	// Retained physical resources publish their own capture state; a Project stopped before creating one has no runtime output.
	if data.state == projectStateStopped && data.physicalUID == "" {
		source := &apiv2.LogSource{Complete: true}
		if !reflect.DeepEqual(resource.Status.RuntimeLogs, source) {
			resource.Status.RuntimeLogs = source
			change |= statusChanged
		}
	}
	commonChange, delay, valid := data.applyCommonTo(resource, projectContainerPortRequests(resource), false)
	return change | commonChange, delay, valid
}
