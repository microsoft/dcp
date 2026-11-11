/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dcptun"
)

type logicalTunnelProxyState string

const (
	logicalTunnelProxyPending  logicalTunnelProxyState = "Pending"
	logicalTunnelProxyStarting logicalTunnelProxyState = "Starting"
	logicalTunnelProxyRunning  logicalTunnelProxyState = "Running"
	logicalTunnelProxyStopping logicalTunnelProxyState = "Stopping"
	logicalTunnelProxyExited   logicalTunnelProxyState = "Exited"
	logicalTunnelProxyFailed   logicalTunnelProxyState = "Failed"
	logicalTunnelProxyUnknown  logicalTunnelProxyState = "Unknown"
)

type logicalTunnelRecord struct {
	status     apiv2.LogicalContainerTunnelStatus
	server     apiv2.ServiceAddress
	attempts   uint32
	retryAfter time.Time
}

type logicalTunnelProxyData struct {
	state   logicalTunnelProxyState
	reason  apiv2.ConditionReason
	message string
	// started records that the proxy pair reached Running, which distinguishes Missing from FailedToStart.
	started              bool
	containerUID         types.UID
	imageUID             types.UID
	processUID           types.UID
	imageBuildPlan       *dcptun.ClientProxyImageBuildPlan
	unobservedChildren   unobservedLogicalChildren
	security             dcptun.TunnelProxySecurityConfig
	control              *dcptun.ReverseControlConnection
	tunnels              map[string]logicalTunnelRecord
	configurationVersion int32
}

func (data *logicalTunnelProxyData) Clone() *logicalTunnelProxyData {
	cloned := *data
	if data.imageBuildPlan != nil {
		plan := *data.imageBuildPlan
		if plan.BuildContextArchive != nil {
			archive := *plan.BuildContextArchive
			plan.BuildContextArchive = &archive
		}
		cloned.imageBuildPlan = &plan
	}
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
	cloned.tunnels = maps.Clone(data.tunnels)
	for name, record := range cloned.tunnels {
		record.status.ClientProxyAddresses = slices.Clone(record.status.ClientProxyAddresses)
		cloned.tunnels[name] = record
	}
	return &cloned
}

func (data *logicalTunnelProxyData) UpdateFrom(other *logicalTunnelProxyData) bool {
	if other == nil {
		return false
	}
	left, right := *data, *other
	left.control, right.control = nil, nil
	if data.control == other.control && reflect.DeepEqual(left, right) {
		return false
	}
	*data = *other.Clone()
	return true
}

func (data *logicalTunnelProxyData) fail(reason apiv2.ConditionReason, message string) {
	data.state, data.reason, data.message = logicalTunnelProxyFailed, reason, message
}

// failMissing reports a backing that is gone or no longer verifiably owned by the proxy.
func (data *logicalTunnelProxyData) failMissing(message string) {
	reason := apiv2.LogicalContainerTunnelProxyReasonFailedToStart
	if data.started {
		reason = apiv2.LogicalContainerTunnelProxyReasonMissing
	}
	data.fail(reason, message)
}

// stopAfterExit begins stopping the remaining backing after a running backing exits on its own.
func (data *logicalTunnelProxyData) stopAfterExit(message string) {
	data.state, data.reason, data.message = logicalTunnelProxyStopping, "", message
}

func applyLogicalTunnelProxyContainerStatus(proxy *apiv2.LogicalContainerTunnelProxy, container *apiv2.PhysicalContainer) objectChange {
	if container == nil {
		return noChange
	}
	change := setValue(&proxy.Status.PhysicalContainerRef, container.Name)
	change |= setValue(&proxy.Status.ContainerID, container.Status.ContainerID)
	return change
}

func applyLogicalTunnelProxyImageStatus(proxy *apiv2.LogicalContainerTunnelProxy, image *apiv2.PhysicalContainerImage) objectChange {
	if image == nil {
		return noChange
	}
	change := setValue(&proxy.Status.PhysicalImageRef, image.Name)
	change |= setValue(&proxy.Status.Image, image.Status.Image)
	return change
}

func applyLogicalTunnelProxyProcessStatus(proxy *apiv2.LogicalContainerTunnelProxy, server *apiv2.PhysicalProcess) objectChange {
	if server == nil {
		return noChange
	}
	change := setValue(&proxy.Status.PhysicalProcessRef, server.Name)
	if !reflect.DeepEqual(proxy.Status.PID, server.Status.PID) {
		proxy.Status.PID = server.Status.DeepCopy().PID
		change |= statusChanged
	}
	return change
}

func (data *logicalTunnelProxyData) applyTo(proxy *apiv2.LogicalContainerTunnelProxy) (objectChange, AdditionalReconciliationDelay, bool) {
	var statuses []apiv2.LogicalContainerTunnelStatus
	for _, record := range data.tunnels {
		statuses = append(statuses, *record.status.DeepCopy())
	}
	slices.SortFunc(statuses, func(left, right apiv2.LogicalContainerTunnelStatus) int {
		return strings.Compare(left.Name, right.Name)
	})
	change := noChange
	if !reflect.DeepEqual(proxy.Status.TunnelStatuses, statuses) {
		proxy.Status.TunnelStatuses = statuses
		change |= statusChanged
	}
	change |= setValue(&proxy.Status.TunnelConfigurationVersion, data.configurationVersion)
	if data.state == logicalTunnelProxyRunning && data.reason == apiv2.LogicalContainerTunnelProxyReasonPaused {
		change |= setValue(&proxy.Status.Phase, apiv2.LogicalContainerTunnelProxyPhaseRunning)
		change |= setCondition(&proxy.Status.Conditions, apiv2.ConditionReady, proxy.Generation,
			metav1.ConditionFalse, data.reason, data.message)
		return change, StandardDelay, true
	}
	projected, delay, valid := logicalTunnelProxyProjections.applyWithReason(data.state, data.reason, data.message,
		&proxy.Status.Phase, &proxy.Status.Conditions, proxy.Generation)
	return change | projected, delay, valid
}

var logicalTunnelProxyProjections = physicalResourceProjectionTable[logicalTunnelProxyState, apiv2.LogicalContainerTunnelProxyPhase]{
	invalidPhase: apiv2.LogicalContainerTunnelProxyPhaseUnknown,
	projections: map[logicalTunnelProxyState]physicalResourceProjection[apiv2.LogicalContainerTunnelProxyPhase]{
		logicalTunnelProxyPending: {phase: apiv2.LogicalContainerTunnelProxyPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerTunnelProxyReasonStarting, message: "Preparing the tunnel proxy image, container, and server process."},
		logicalTunnelProxyStarting: {phase: apiv2.LogicalContainerTunnelProxyPhasePending, conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.LogicalContainerTunnelProxyReasonStarting, message: "Waiting for the host tunnel server and authenticated control connection."},
		logicalTunnelProxyRunning:  {phase: apiv2.LogicalContainerTunnelProxyPhaseRunning, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.LogicalContainerTunnelProxyReasonRunning},
		logicalTunnelProxyStopping: {phase: apiv2.LogicalContainerTunnelProxyPhaseRunning, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerTunnelProxyReasonStopping},
		logicalTunnelProxyExited:   {phase: apiv2.LogicalContainerTunnelProxyPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerTunnelProxyReasonCompleted},
		logicalTunnelProxyFailed:   {phase: apiv2.LogicalContainerTunnelProxyPhaseFailed, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerTunnelProxyReasonFailedToStart},
		logicalTunnelProxyUnknown:  {phase: apiv2.LogicalContainerTunnelProxyPhaseUnknown, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.LogicalContainerTunnelProxyReasonLookupFailed},
	},
}
