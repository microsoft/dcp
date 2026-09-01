/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

type physicalResourceProgress int

const (
	physicalResourceProgressInProgress physicalResourceProgress = iota + 1
	physicalResourceProgressCompleted
	physicalResourceProgressRetryPending
	physicalResourceProgressFailed
	physicalResourceProgressNotFound
	physicalResourceProgressNotReady
	physicalResourceProgressTerminating
	physicalResourceProgressNotActive
	physicalResourceProgressRunning
	physicalResourceProgressExited
	physicalResourceProgressDead
	physicalResourceProgressPaused
	physicalResourceProgressRestarting
	physicalResourceProgressCreated
	physicalResourceProgressRemoving
	physicalResourceProgressMissing
	physicalResourceProgressUnknown
	physicalResourceProgressAbandoned
	physicalResourceProgressSkipped
	physicalResourceProgressResultMissing
)

type physicalResourceProjectionKey[State comparable] struct {
	state    State
	progress physicalResourceProgress
}

type physicalResourceProjection[Phase ~string] struct {
	phase           Phase
	conditionStatus metav1.ConditionStatus
	conditionReason apiv2.ConditionReason
	message         string
	requeue         bool
	requeueDelay    AdditionalReconciliationDelay
}

type physicalResourceProjectionTable[State comparable, Phase ~string] struct {
	projections  map[physicalResourceProjectionKey[State]]physicalResourceProjection[Phase]
	invalidPhase Phase
}

func (table physicalResourceProjectionTable[State, Phase]) project(
	state State,
	progress physicalResourceProgress,
) (physicalResourceProjection[Phase], bool) {
	projection, found := table.projections[physicalResourceProjectionKey[State]{
		state:    state,
		progress: progress,
	}]
	return projection, found
}

func (table physicalResourceProjectionTable[State, Phase]) reconciliationDelay(
	state State,
	progress physicalResourceProgress,
) AdditionalReconciliationDelay {
	projection, valid := table.project(state, progress)
	if !valid {
		return LongDelay
	}
	return projection.requeueDelay
}

func (table physicalResourceProjectionTable[State, Phase]) apply(
	state State,
	progress physicalResourceProgress,
	message string,
	phase *Phase,
	conditions *[]metav1.Condition,
	generation int64,
) (objectChange, AdditionalReconciliationDelay, bool) {
	projection, valid := table.project(state, progress)
	if !valid {
		projection = physicalResourceProjection[Phase]{
			phase:           table.invalidPhase,
			conditionStatus: metav1.ConditionFalse,
			conditionReason: apiv2.PhysicalResourceReasonOperationStateInvalid,
			message:         fmt.Sprintf("Physical resource reached invalid reconciliation state %v with progress %v.", state, progress),
			requeue:         true,
			requeueDelay:    LongDelay,
		}
	} else if message != "" {
		projection.message = message
	}

	change := setValue(phase, projection.phase)
	change |= setCondition(
		conditions,
		apiv2.ConditionReady,
		generation,
		projection.conditionStatus,
		projection.conditionReason,
		projection.message,
	)
	if projection.requeue {
		change |= additionalReconciliationNeeded
	}
	return change, projection.requeueDelay, valid
}
