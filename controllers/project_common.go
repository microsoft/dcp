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
	"os"
	"slices"
	"strings"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	templating "github.com/microsoft/dcp/internal/templating"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_maps "github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

type projectState int

const (
	projectStatePending projectState = iota + 1
	projectStateCreating
	projectStateAwaitingStart
	projectStateReady
	projectStateActive
	projectStateRunning
	projectStateExited
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
	case projectStateAwaitingStart:
		return "AwaitingStart"
	case projectStateReady:
		return "Ready"
	case projectStateActive:
		return "Active"
	case projectStateRunning:
		return "Running"
	case projectStateExited:
		return "Exited"
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

// projectData is the controller-owned state shared by ProjectProcess and ProjectContainer.
type projectData struct {
	state              projectState
	reason             apiv2.ConditionReason
	message            string
	ports              []logicalPortSelection
	unobservedChildren unobservedLogicalChildren
	effectiveArgs      []string
	effectiveEnv       []commonapi.EnvVar
	finishedAt         metav1.MicroTime
	exitCode           *int32
	inputsPrepared     bool
	// Records successful preparation independently of temporary observation failures.
	prepared    bool
	processes   map[process.ProcessHandle]apiv2.ProjectDebugProcessObservation
	processPoll bool
}

func (data *projectData) cloneProjectData() projectData {
	cloned := *data
	cloned.processes = maps.Clone(data.processes)
	cloned.ports = slices.Clone(data.ports)
	cloned.unobservedChildren = maps.Clone(data.unobservedChildren)
	cloned.effectiveArgs = slices.Clone(data.effectiveArgs)
	cloned.effectiveEnv = slices.Clone(data.effectiveEnv)
	cloned.exitCode = cloneInt32Pointer(data.exitCode)
	return cloned
}

func (data *projectData) failMissing(message string) {
	data.state, data.reason, data.message = projectStateFailed, apiv2.ProjectReasonPreparationFailed, message
	if data.prepared {
		data.reason = apiv2.ProjectReasonMissing
	}
}

func (data *projectData) requestStop() bool {
	switch data.state {
	case projectStatePending, projectStateCreating, projectStateAwaitingStart, projectStateReady, projectStateActive,
		projectStateRunning, projectStateExited, projectStatePaused, projectStateUnknown:
		data.state, data.reason, data.message = projectStateStopping, apiv2.ProjectReasonStopping, ""
		return true
	default:
		return false
	}
}

func (data *projectData) portDataFor(owner ctrl_client.Object, requests []logicalPortRequest) []logicalPortData {
	var ports []logicalPortData
	for portIndex, selected := range data.ports {
		ports = append(ports, logicalPortForSelection(owner, requests[portIndex], selected))
	}
	return ports
}

// applyCommonTo projects shared Project status; the launch environment is projected only when processEnvironment is set.
func (data *projectData) applyCommonTo(
	resource apiv2.ProjectObject, requests []logicalPortRequest, processEnvironment bool,
) (objectChange, AdditionalReconciliationDelay, bool) {
	status := resource.GetProjectStatus()
	change := projectLogicalPorts(data.portDataFor(resource, requests), &status.Ports)
	change |= projectProcessObservations(status, data)
	if !slices.Equal(status.EffectiveArgs, data.effectiveArgs) {
		status.EffectiveArgs = slices.Clone(data.effectiveArgs)
		change |= statusChanged
	}
	if processEnvironment && !slices.Equal(status.EffectiveEnv, data.effectiveEnv) {
		status.EffectiveEnv = slices.Clone(data.effectiveEnv)
		change |= statusChanged
	}
	change |= setTimestamp(&status.FinishedAt, data.finishedAt)
	change |= setExitCode(&status.ExitCode, data.exitCode)
	change |= setValue(&status.SourceDirectory, resource.GetSourceDirectory())
	projectionChange, delay, valid := projectProjections.applyWithReason(
		data.state, data.reason, data.message, &status.Phase, &status.Conditions, resource.GetGeneration())
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
		projectStateAwaitingStart: {
			phase: apiv2.ProjectPhaseReady, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonAwaitingStart,
			message: "Project execution is waiting for start to become true.",
		},
		projectStateReady: {
			phase: apiv2.ProjectPhaseReady, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonPrepared,
		},
		projectStateActive: {
			phase: apiv2.ProjectPhaseActive, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonDebugSessionsActive,
		},
		projectStateRunning: {
			phase: apiv2.ProjectPhaseActive, conditionStatus: metav1.ConditionTrue, conditionReason: apiv2.ProjectReasonApplicationRunning,
		},
		projectStateExited: {
			phase: apiv2.ProjectPhaseExited, conditionStatus: metav1.ConditionFalse, conditionReason: apiv2.ProjectReasonCompleted,
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

// checkProjectNamespace reports namespace readiness before Project state is created.
func checkProjectNamespace(ctx context.Context, reader ctrl_client.Reader, resource apiv2.ProjectObject, log logr.Logger) (objectChange, bool) {
	ready, reason, namespaceErr := checkNamespaceReadiness(ctx, reader, resource.GetNamespace())
	if ready {
		return noChange, true
	}
	status := resource.GetProjectStatus()
	change := noChange
	message := namespaceReadinessMessage(resource.GetNamespace(), reason)
	if namespaceErr != nil {
		log.Error(namespaceErr, "Failed to get Project namespace")
		message = fmt.Sprintf("Failed to get namespace: %v", namespaceErr)
		change |= additionalReconciliationNeeded
	}
	change |= setValue(&status.Phase, apiv2.ProjectPhasePending)
	change |= setCondition(&status.Conditions, apiv2.ConditionReady, resource.GetGeneration(), metav1.ConditionFalse, apiv2.ProjectReasonPreparing, message)
	return change, false
}

// prepareProjectValues allocates ports and evaluates launch values once. checkPorts may reject prepared ports
// with an error wrapping errLogicalValuePreparationFailed; the evaluated values are returned for kind-specific processing.
func prepareProjectValues(
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, resource apiv2.ProjectObject, data *projectData,
	requests []logicalPortRequest, arguments []string, environment []commonapi.EnvVar, processLaunch bool,
	checkPorts func([]logicalPortData) error, log logr.Logger,
) ([]logicalPortData, []string, []commonapi.EnvVar, error) {
	ports, prepareErr := prepareLogicalPorts(ctx, apiClient, reader, resource, requests, log)
	if prepareErr != nil {
		log.Error(prepareErr, "Failed to prepare Project ports")
		data.reason, data.message = apiv2.ProjectReasonPreparing, prepareErr.Error()
		if errors.Is(prepareErr, errLogicalServiceDependencyFailed) {
			data.reason = apiv2.ProjectReasonPreparationFailed
			return nil, nil, nil, fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, prepareErr)
		}
		return nil, nil, nil, prepareErr
	}
	if checkPorts != nil {
		if checkErr := checkPorts(ports); checkErr != nil {
			return nil, nil, nil, checkErr
		}
	}
	resolvedArgs, resolvedEnv, evaluationErr := templating.EvaluateV2Values(ctx, reader, resource,
		servingPorts(ports), arguments, environment, processLaunch && osutil.IsWindows())
	if evaluationErr != nil {
		log.Error(evaluationErr, "Failed to evaluate Project launch values")
		data.reason, data.message = apiv2.ProjectReasonPreparationFailed, evaluationErr.Error()
		if !templating.IsTransientTemplateError(evaluationErr) {
			return nil, nil, nil, fmt.Errorf("%w: %w", errLogicalValuePreparationFailed, evaluationErr)
		}
		data.reason = apiv2.ProjectReasonPreparing
		return nil, nil, nil, evaluationErr
	}
	return ports, resolvedArgs, resolvedEnv, nil
}

// mergeProjectAmbientEnvironment overlays evaluated declarations onto literal host environment values.
func mergeProjectAmbientEnvironment(declared []commonapi.EnvVar) []commonapi.EnvVar {
	mode := usvc_maps.StringMapModeCaseSensitive
	if osutil.IsWindows() {
		mode = usvc_maps.StringMapModeCaseInsensitive
	}
	merged := usvc_maps.NewStringKeyMap[string](mode)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found {
			merged.Override(name, value)
		}
	}
	for _, variable := range declared {
		merged.Override(variable.Name, variable.Value)
	}
	return usvc_maps.MapToSlice[commonapi.EnvVar](merged.Data(), func(name, value string) commonapi.EnvVar {
		return commonapi.EnvVar{Name: name, Value: value}
	})
}

// projectLaunchedSessionReady reports whether a connected session is ready and has requested launch.
func projectLaunchedSessionReady(status *apiv2.ProjectCommonStatus) bool {
	if status.Debug == nil {
		return false
	}
	for _, session := range status.Debug.Sessions {
		if session.Phase == apiv2.ProjectDebugPhaseRunning && session.Progress != nil && session.Progress.LaunchRequested &&
			apimeta.IsStatusConditionTrue(session.Conditions, string(apiv2.ConditionReady)) {
			return true
		}
	}
	return false
}

func projectDebugSessionsActive(status *apiv2.ProjectCommonStatus) bool {
	if status.Debug != nil {
		for _, session := range status.Debug.Sessions {
			if session.Phase == apiv2.ProjectDebugPhasePending || session.Phase == apiv2.ProjectDebugPhaseRunning {
				return true
			}
		}
	}
	return false
}

// syncProjectEndpoints publishes endpoints while a direct application is running or an adapter-launched session is ready.
func syncProjectEndpoints[Resource apiv2.ProjectObject](
	ctx context.Context, apiClient ctrl_client.Client, reader ctrl_client.Reader, resource Resource, data *projectData,
	ports logicalPortDataSource[Resource], requests []logicalPortRequest, ownerUIDField string,
	mappings []apiv2.PhysicalContainerPortMapping, log logr.Logger,
) objectChange {
	if len(requests) == 0 {
		return noChange
	}
	publish := resource.GetProjectSpec().ShouldStart() && data.inputsPrepared &&
		(data.state == projectStateRunning || (data.state == projectStateActive && projectLaunchedSessionReady(resource.GetProjectStatus())))
	addressesReady, endpointErr := syncLogicalEndpoints(ctx, apiClient, reader, resource, ports, ownerUIDField,
		&data.unobservedChildren, publish, mappings...)
	if endpointErr != nil {
		log.Error(endpointErr, "Failed to synchronize Project endpoints", "Publish", publish)
		return additionalReconciliationNeeded
	}
	if publish && !addressesReady {
		data.message = "Waiting for container port mappings to publish service endpoints."
		return additionalReconciliationNeeded
	}
	return noChange
}

func handleInvalidProjectState(data *projectData, log logr.Logger) objectChange {
	stateErr := fmt.Errorf("invalid project state %v", data.state)
	log.Error(stateErr, "Project reached invalid reconciliation state")
	data.state, data.reason, data.message = projectStateFailed, apiv2.PhysicalResourceReasonOperationStateInvalid, stateErr.Error()
	return noChange
}
