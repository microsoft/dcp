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

func TestPhysicalResourceProjections(t *testing.T) {
	t.Parallel()

	assertPhysicalResourceProjections(t, physicalContainerProjections)
	assertPhysicalResourceProjections(t, physicalContainerImageProjections)
	assertPhysicalResourceProjections(t, physicalContainerNetworkProjections)
	assertPhysicalResourceProjections(t, physicalContainerVolumeProjections)
	assertPhysicalResourceProjections(t, physicalProcessProjections)
}

func TestPhysicalResourceProjectionsRejectInvalidCombination(t *testing.T) {
	t.Parallel()

	projections := physicalResourceProjectionTable[int, string]{
		invalidPhase: "Unknown",
		projections: map[physicalResourceProjectionKey[int]]physicalResourceProjection[string]{
			{state: 1, progress: physicalResourceProgressCompleted}: {
				phase:           "Ready",
				conditionStatus: metav1.ConditionTrue,
				conditionReason: "Completed",
			},
		},
	}

	phase := ""
	conditions := []metav1.Condition{}
	change, delay, valid := projections.apply(
		1,
		physicalResourceProgressFailed,
		"",
		&phase,
		&conditions,
		1,
	)

	require.False(t, valid)
	require.Equal(t, LongDelay, delay)
	require.Equal(t, "Unknown", phase)
	require.Len(t, conditions, 1)
	require.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	require.Equal(t, string(apiv2.PhysicalResourceReasonOperationStateInvalid), conditions[0].Reason)
	require.NotEqual(t, noChange, change&statusChanged)
	require.NotEqual(t, noChange, change&additionalReconciliationNeeded)
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
			key.state,
			key.progress,
			"",
			&phase,
			&conditions,
			1,
		)

		require.True(t, valid, "state %v, progress %v", key.state, key.progress)
		require.Equal(t, expected.phase, phase, "state %v, progress %v", key.state, key.progress)
		require.Equal(t, expected.requeueDelay, delay, "state %v, progress %v", key.state, key.progress)
		require.Len(t, conditions, 1, "state %v, progress %v", key.state, key.progress)
		require.Equal(t, expected.conditionStatus, conditions[0].Status, "state %v, progress %v", key.state, key.progress)
		require.Equal(t, string(expected.conditionReason), conditions[0].Reason, "state %v, progress %v", key.state, key.progress)
		require.NotEqual(t, noChange, change&statusChanged, "state %v, progress %v", key.state, key.progress)
		if expected.requeue {
			require.NotEqual(t, noChange, change&additionalReconciliationNeeded, "state %v, progress %v", key.state, key.progress)
		} else {
			require.Equal(t, noChange, change&additionalReconciliationNeeded, "state %v, progress %v", key.state, key.progress)
		}
	}
}
