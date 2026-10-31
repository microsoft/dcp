/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

// Verifies that physical resource projections preserve phase, conditions, and retry policy.
// Terminal dependency failures and confirmed missing containers and networks never schedule retries.
func TestPhysicalResourceProjections(t *testing.T) {
	t.Parallel()

	assertPhysicalResourceProjections(t, physicalContainerProjections)
	assertPhysicalResourceProjections(t, physicalContainerImageProjections)
	assertPhysicalResourceProjections(t, physicalContainerNetworkProjections)
	assertPhysicalResourceProjections(t, physicalContainerVolumeProjections)
	assertPhysicalResourceProjections(t, physicalProcessProjections)
	assertPhysicalResourceProjections(t, logicalContainerProjections)
	assertPhysicalResourceProjections(t, logicalContainerNetworkProjections)

	containerMissing, containerMissingFound := physicalContainerProjections.project(physicalContainerStateMissing)
	require.True(t, containerMissingFound)
	require.False(t, containerMissing.requeue)
	networkMissing, networkMissingFound := physicalContainerNetworkProjections.project(physicalContainerNetworkStateMissing)
	require.True(t, networkMissingFound)
	require.False(t, networkMissing.requeue)
	dependencyFailed, dependencyFailedFound := physicalContainerProjections.project(physicalContainerStateDependencyFailed)
	require.True(t, dependencyFailedFound)
	require.Equal(t, apiv2.PhysicalContainerPhaseFailed, dependencyFailed.phase)
	require.False(t, dependencyFailed.requeue)
}

// Verifies that every projected normal state has a reconciliation handler.
func TestV2ProjectedStatesHaveHandlers(t *testing.T) {
	t.Parallel()

	assertProjectedStateHandlers(t, physicalContainerProjections, physicalContainerDataInitializers)
	assertProjectedStateHandlers(t, physicalContainerImageProjections, physicalContainerImageDataHandlers)
	assertProjectedStateHandlers(t, physicalContainerNetworkProjections, physicalContainerNetworkDataInitializers)
	assertProjectedStateHandlers(t, physicalContainerVolumeProjections, physicalContainerVolumeDataInitializers)
	assertProjectedStateHandlers(t, physicalProcessProjections, physicalProcessDataHandlers)
	for state := range logicalContainerProjections.projections {
		if state == logicalContainerStateDelete {
			continue
		}
		require.Contains(t, logicalContainerDataHandlers, state)
	}
	for state := range logicalContainerNetworkProjections.projections {
		if state == logicalContainerNetworkStateDelete {
			continue
		}
		require.Contains(t, logicalContainerNetworkDataHandlers, state)
	}
}

// Verifies that action states alone do not claim outstanding work; only queued asynchronous work does.
func TestV2OutstandingWorkTracking(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		idle interface{ operationInProgress() bool }
		busy interface{ operationInProgress() bool }
	}{
		{"container", &physicalContainerData{state: physicalContainerStateCreate},
			&physicalContainerData{state: physicalContainerStateCreate, workInProgress: true}},
		{"image", &physicalContainerImageData{state: physicalContainerImageStatePull},
			&physicalContainerImageData{state: physicalContainerImageStatePull, workInProgress: true}},
		{"network", &physicalContainerNetworkData{state: physicalContainerNetworkStateCreate},
			&physicalContainerNetworkData{state: physicalContainerNetworkStateCreate, workInProgress: true}},
		{"volume", &physicalContainerVolumeData{state: physicalContainerVolumeStateRemove},
			&physicalContainerVolumeData{state: physicalContainerVolumeStateRemove, workInProgress: true}},
		{"process", &physicalProcessData{state: physicalProcessStateStop},
			&physicalProcessData{state: physicalProcessStateStop, workInProgress: true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.False(t, testCase.idle.operationInProgress())
			require.True(t, testCase.busy.operationInProgress())
		})
	}
}

// Verifies that state diagnostics distinguish unset, known, and unsupported states.
func TestPhysicalResourceStateStringsDistinguishUnsetAndInvalidValues(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Unset", physicalContainerState(0).String())
	require.Equal(t, "Dependencies", physicalContainerStateDependencies.String())
	require.Equal(t, "DependencyFailed", physicalContainerStateDependencyFailed.String())
	require.Equal(t, "DependencyLookupFailed", physicalContainerStateDependencyLookupFailed.String())
	require.Equal(t, "physicalContainerState(999)", physicalContainerState(999).String())
	require.Equal(t, "Unset", physicalContainerImageState(0).String())
	require.Equal(t, "physicalContainerImageState(999)", physicalContainerImageState(999).String())
	require.Equal(t, "Unset", physicalContainerNetworkState(0).String())
	require.Equal(t, "physicalContainerNetworkState(999)", physicalContainerNetworkState(999).String())
	require.Equal(t, "Unset", physicalContainerVolumeState(0).String())
	require.Equal(t, "physicalContainerVolumeState(999)", physicalContainerVolumeState(999).String())
	require.Equal(t, "Unset", physicalProcessState(0).String())
	require.Equal(t, "physicalProcessState(999)", physicalProcessState(999).String())
	require.Equal(t, "Unset", logicalContainerState(0).String())
	require.Equal(t, "Pending", logicalContainerStatePending.String())
	require.Equal(t, "Creating", logicalContainerStateCreating.String())
	require.Equal(t, "logicalContainerState(999)", logicalContainerState(999).String())
}

// Verifies that an unsupported state produces a terminal invalid-state diagnostic.
func TestPhysicalResourceProjectionsRejectUnknownState(t *testing.T) {
	t.Parallel()

	projections := physicalResourceProjectionTable[physicalProcessState, string]{
		invalidPhase: "Unknown",
		projections: map[physicalProcessState]physicalResourceProjection[string]{
			physicalProcessStateResolve: {
				phase:           "Ready",
				conditionStatus: metav1.ConditionTrue,
				conditionReason: "Completed",
			},
		},
	}

	phase := ""
	conditions := []metav1.Condition{}
	change, delay, valid := projections.apply(
		physicalProcessState(999),
		"",
		&phase,
		&conditions,
		1,
	)

	require.False(t, valid)
	require.Equal(t, StandardDelay, delay)
	require.Equal(t, "Unknown", phase)
	require.Len(t, conditions, 1)
	require.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	require.Equal(t, string(apiv2.PhysicalResourceReasonOperationStateInvalid), conditions[0].Reason)
	require.Equal(t, "Physical resource reached invalid reconciliation state physicalProcessState(999).", conditions[0].Message)
	require.NotEqual(t, noChange, change&statusChanged)
	require.Equal(t, noChange, change&additionalReconciliationNeeded)
}

// Verifies that invalid-state projections do not request further reconciliation.
func TestPhysicalResourceInvalidProjectionsAreTerminal(t *testing.T) {
	t.Parallel()

	assertTerminalPhysicalResourceProjection(t, physicalContainerProjections, physicalContainerStateInvalid)
	assertTerminalPhysicalResourceProjection(t, physicalContainerImageProjections, physicalContainerImageStateInvalid)
	assertTerminalPhysicalResourceProjection(t, physicalContainerNetworkProjections, physicalContainerNetworkStateInvalid)
	assertTerminalPhysicalResourceProjection(t, physicalContainerVolumeProjections, physicalContainerVolumeStateInvalid)
	assertTerminalPhysicalResourceProjection(t, physicalProcessProjections, physicalProcessStateInvalid)
}

// Verifies that a state projection permits a more specific diagnostic condition reason.
func TestPhysicalResourceProjectionsOverrideConditionReason(t *testing.T) {
	t.Parallel()

	projections := physicalResourceProjectionTable[int, string]{
		invalidPhase: "Unknown",
		projections: map[int]physicalResourceProjection[string]{
			1: {
				phase:           "Pending",
				conditionStatus: metav1.ConditionFalse,
				conditionReason: "DefaultReason",
			},
		},
	}

	phase := ""
	conditions := []metav1.Condition{}
	_, _, valid := projections.applyWithReason(
		1,
		"SpecificReason",
		"",
		&phase,
		&conditions,
		1,
	)

	require.True(t, valid)
	require.Len(t, conditions, 1)
	require.Equal(t, "SpecificReason", conditions[0].Reason)
}

func assertPhysicalResourceProjections[State comparable, Phase ~string](
	t *testing.T,
	projections physicalResourceProjectionTable[State, Phase],
) {
	t.Helper()

	require.NotEmpty(t, projections.projections)
	for key, expected := range projections.projections {
		phase := Phase("")
		conditions := []metav1.Condition{}
		change, delay, valid := projections.apply(
			key,
			"",
			&phase,
			&conditions,
			1,
		)

		require.True(t, valid, "state %v", key)
		require.Equal(t, expected.phase, phase, "state %v", key)
		require.Equal(t, expected.requeueDelay, delay, "state %v", key)
		require.Len(t, conditions, 1, "state %v", key)
		require.Equal(t, expected.conditionStatus, conditions[0].Status, "state %v", key)
		require.Equal(t, string(expected.conditionReason), conditions[0].Reason, "state %v", key)
		require.NotEqual(t, noChange, change&statusChanged, "state %v", key)
		if expected.requeue {
			require.NotEqual(t, noChange, change&additionalReconciliationNeeded, "state %v", key)
		} else {
			require.Equal(t, noChange, change&additionalReconciliationNeeded, "state %v", key)
		}
	}
}

func assertProjectedStateHandlers[State comparable, Phase ~string, Handler any](
	t *testing.T,
	projections physicalResourceProjectionTable[State, Phase],
	handlers map[State]Handler,
) {
	t.Helper()
	for state := range projections.projections {
		require.Contains(t, handlers, state, "state %v has no reconciliation handler", state)
	}
}

func assertTerminalPhysicalResourceProjection[State comparable, Phase ~string](
	t *testing.T,
	projections physicalResourceProjectionTable[State, Phase],
	invalidState State,
) {
	t.Helper()

	projection, found := projections.project(invalidState)
	require.True(t, found)
	require.False(t, projection.requeue)
	require.Equal(t, StandardDelay, projection.requeueDelay)
	require.Equal(t, apiv2.PhysicalResourceReasonOperationStateInvalid, projection.conditionReason)
}
