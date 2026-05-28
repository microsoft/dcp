/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/pointers"
)

// IdeSessionRunInfo stores in-memory state about an IdeSession run.
// It mirrors (a subset of) IdeSession.Status and is the source of truth for state
// transitions because the in-memory state is immune to client-side cache staleness
// and to optimistic-concurrency conflicts on the API object update path.
type IdeSessionRunInfo struct {
	// UID of the IdeSession this run info belongs to.
	UID types.UID

	// State is the most recently observed state of the IDE run session.
	State apiv1.IdeSessionState

	// SessionID is the identifier the IDE returned for the run session.
	SessionID string

	// PID is the PID the IDE reported for the main process of the session.
	PID *int64

	// ExitCode is the exit code reported by the IDE when the session terminated.
	ExitCode *int32

	// Message is human-readable detail (typically populated when the session enters Failed).
	Message string

	// Paths to the stdout/stderr capture files.
	StdOutFile string
	StdErrFile string

	// Timestamps for the lifecycle events.
	StartupTimestamp metav1.MicroTime
	FinishTimestamp  metav1.MicroTime

	// stopAttemptInitiated is a one-way latch indicating that the controller has
	// already asked the IDE to stop the session. It prevents the controller from
	// issuing duplicate stop requests while a stop is in progress.
	stopAttemptInitiated bool

	// startQueued is a one-way latch indicating that the controller has already
	// dispatched the asynchronous start operation for the session. It prevents
	// the controller from queuing the start more than once.
	startQueued bool
}

// NewIdeSessionRunInfo returns a fresh run info populated with the IdeSession UID
// (which is the only field that does not depend on per-run progress).
func NewIdeSessionRunInfo(s *apiv1.IdeSession) *IdeSessionRunInfo {
	return &IdeSessionRunInfo{
		UID:      s.UID,
		State:    apiv1.IdeSessionStateInitial,
		ExitCode: apiv1.UnknownExitCode,
	}
}

// GetResourceId implements the per-run resource identifier used by the resource log sink.
func (ri *IdeSessionRunInfo) GetResourceId() string {
	return fmt.Sprintf("idesession-%s", ri.UID)
}

// Clone returns a deep copy of the run info (sufficient for the way ObjectStateMap stores values).
func (ri *IdeSessionRunInfo) Clone() *IdeSessionRunInfo {
	retval := &IdeSessionRunInfo{
		UID:                  ri.UID,
		State:                ri.State,
		SessionID:            ri.SessionID,
		Message:              ri.Message,
		StdOutFile:           ri.StdOutFile,
		StdErrFile:           ri.StdErrFile,
		StartupTimestamp:     ri.StartupTimestamp,
		FinishTimestamp:      ri.FinishTimestamp,
		stopAttemptInitiated: ri.stopAttemptInitiated,
		startQueued:          ri.startQueued,
	}
	if ri.PID != apiv1.UnknownPID {
		pointers.SetValueFrom(&retval.PID, ri.PID)
	}
	if ri.ExitCode != apiv1.UnknownExitCode {
		pointers.SetValueFrom(&retval.ExitCode, ri.ExitCode)
	}
	return retval
}

// UpdateFrom merges fields from "other" into the receiver. Returns true if anything changed.
// State transitions are validated against IdeSessionState.CanUpdateTo. Out-of-order or stale
// notifications that imply an invalid transition are dropped so they do not undo a more
// recent (and valid) state change.
func (ri *IdeSessionRunInfo) UpdateFrom(other *IdeSessionRunInfo) bool {
	if other == nil {
		return false
	}

	updated := false

	if ri.State != other.State {
		if ri.State.CanUpdateTo(other.State) {
			ri.State = other.State
			updated = true
		} else {
			// Drop the entire update if the state transition would be invalid.
			return false
		}
	}

	if other.UID != "" && ri.UID != other.UID {
		ri.UID = other.UID
		updated = true
	}

	if other.SessionID != "" && ri.SessionID != other.SessionID {
		ri.SessionID = other.SessionID
		updated = true
	}

	if other.PID != apiv1.UnknownPID && !pointers.EqualValue(ri.PID, other.PID) {
		pointers.SetValueFrom(&ri.PID, other.PID)
		updated = true
	} else if other.State.IsTerminal() && ri.PID != apiv1.UnknownPID {
		// Clear PID once the session has finished.
		ri.PID = apiv1.UnknownPID
		updated = true
	}

	if other.ExitCode != apiv1.UnknownExitCode && !pointers.EqualValue(ri.ExitCode, other.ExitCode) {
		pointers.SetValueFrom(&ri.ExitCode, other.ExitCode)
		updated = true
	}

	if other.Message != "" && ri.Message != other.Message {
		ri.Message = other.Message
		updated = true
	}

	if other.StdOutFile != "" && ri.StdOutFile != other.StdOutFile {
		ri.StdOutFile = other.StdOutFile
		updated = true
	}

	if other.StdErrFile != "" && ri.StdErrFile != other.StdErrFile {
		ri.StdErrFile = other.StdErrFile
		updated = true
	}

	updated = setTimestampIfAfterOrUnknown(other.StartupTimestamp, &ri.StartupTimestamp) || updated
	updated = setTimestampIfAfterOrUnknown(other.FinishTimestamp, &ri.FinishTimestamp) || updated

	if other.stopAttemptInitiated && !ri.stopAttemptInitiated {
		ri.stopAttemptInitiated = true
		updated = true
	}

	if other.startQueued && !ri.startQueued {
		ri.startQueued = true
		updated = true
	}

	return updated
}

// ApplyTo copies fields from the run info onto the supplied IdeSession's status.
// Returns the kind of object change (statusChanged or noChange) suitable for the
// reconciler's bookkeeping.
func (ri *IdeSessionRunInfo) ApplyTo(s *apiv1.IdeSession, log logr.Logger) objectChange {
	status := &s.Status
	change := noChange

	if ri.State != apiv1.IdeSessionStateInitial && status.State != ri.State {
		status.State = ri.State
		change = statusChanged
	}

	if ri.SessionID != "" && status.SessionID != ri.SessionID {
		status.SessionID = ri.SessionID
		change = statusChanged
	}

	if ri.PID != apiv1.UnknownPID && (status.PID == nil || *status.PID != *ri.PID) {
		pointers.SetValueFrom(&status.PID, ri.PID)
		change = statusChanged
	} else if ri.State.IsTerminal() && status.PID != nil {
		status.PID = nil
		change = statusChanged
	}

	if ri.ExitCode != apiv1.UnknownExitCode && (status.ExitCode == nil || *status.ExitCode != *ri.ExitCode) {
		pointers.SetValueFrom(&status.ExitCode, ri.ExitCode)
		change = statusChanged
	}

	if ri.Message != "" && status.Message != ri.Message {
		status.Message = ri.Message
		change = statusChanged
	}

	if ri.StdOutFile != "" && status.StdOutFile != ri.StdOutFile {
		status.StdOutFile = ri.StdOutFile
		change = statusChanged
	}

	if ri.StdErrFile != "" && status.StdErrFile != ri.StdErrFile {
		status.StdErrFile = ri.StdErrFile
		change = statusChanged
	}

	if setTimestampIfAfterOrUnknown(ri.StartupTimestamp, &status.StartupTimestamp) {
		change = statusChanged
	}

	if setTimestampIfAfterOrUnknown(ri.FinishTimestamp, &status.FinishTimestamp) {
		change = statusChanged
	}

	if change != noChange && log.V(1).Enabled() {
		log.V(1).Info("IdeSession run changed", "CurrentRunInfo", ri.String())
	}

	return change
}

func (ri *IdeSessionRunInfo) String() string {
	return fmt.Sprintf(
		"{state=%s, sessionID=%s, pid=%s, exitCode=%s, message=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s, stopAttemptInitiated=%t, startQueued=%t}",
		ri.State,
		logger.FriendlyString(ri.SessionID),
		logger.IntPtrValToString(ri.PID),
		logger.IntPtrValToString(ri.ExitCode),
		logger.FriendlyString(ri.Message),
		logger.FriendlyMetav1Timestamp(ri.StartupTimestamp),
		logger.FriendlyMetav1Timestamp(ri.FinishTimestamp),
		logger.FriendlyString(ri.StdOutFile),
		logger.FriendlyString(ri.StdErrFile),
		ri.stopAttemptInitiated,
		ri.startQueued,
	)
}

var _ fmt.Stringer = (*IdeSessionRunInfo)(nil)
var _ Cloner[*IdeSessionRunInfo] = (*IdeSessionRunInfo)(nil)
var _ UpdateableFrom[*IdeSessionRunInfo] = (*IdeSessionRunInfo)(nil)
