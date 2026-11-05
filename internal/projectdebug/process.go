/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/resiliency"
)

func (writer *projectDebugStatusWriter) registerProcess(
	ctx context.Context, name types.NamespacedName, uid types.UID, sessionID string, registration *apiv2.ProjectDebugProcessStatus,
) (*apiv2.Project, error) {
	snapshot := registration.DeepCopy()
	if snapshot != nil && snapshot.ID == "" {
		return nil, fmt.Errorf("process registration ID is required")
	}
	return writer.mutate(ctx, name, uid, func(project *apiv2.Project) error {
		if project.Status.Debug == nil {
			return fmt.Errorf("debug session %q is not registered", sessionID)
		}
		index := slices.IndexFunc(project.Status.Debug.Sessions, func(session apiv2.ProjectDebugSessionStatus) bool { return session.ID == sessionID })
		if index < 0 {
			return fmt.Errorf("debug session %q is not registered", sessionID)
		}
		session := &project.Status.Debug.Sessions[index]
		if snapshot != nil {
			for _, existing := range session.Processes {
				if existing.ID == snapshot.ID || (existing.PID != nil && snapshot.PID != nil &&
					!existing.IdentityTimestamp.IsZero() && existing.IdentityTimestamp.Equal(&snapshot.IdentityTimestamp) &&
					*existing.PID == *snapshot.PID) {
					return nil
				}
			}
			entry := snapshot.DeepCopy()
			entry.Observation = apiv2.ProjectDebugProcessObservation{}
			session.Processes = append(session.Processes, *entry)
		}
		session.ProcessTrackingUnconfirmed, session.ProcessTrackingMessage = false, ""
		if len(session.Processes) == 0 {
			session.ProcessTrackingUnconfirmed = true
			session.ProcessTrackingMessage = "The adapter has not reported a usable local target process identity."
		}
		for _, registered := range session.Processes {
			if registered.TrackingError != "" {
				session.ProcessTrackingUnconfirmed = true
				session.ProcessTrackingMessage = registered.TrackingError
				break
			}
		}
		return nil
	})
}

func (runtime *projectDebugRuntime) observeProcessMessage(project *apiv2.Project, sessionID string, message dap.Message) error {
	var kind, event, command string
	if decodeErr := json.Unmarshal(message["type"], &kind); decodeErr != nil {
		return fmt.Errorf("decode DAP process message type: %w", decodeErr)
	}
	if kind == "response" {
		if commandErr := json.Unmarshal(message["command"], &command); commandErr != nil {
			return fmt.Errorf("decode DAP response command: %w", commandErr)
		}
		if command != "launch" && command != "attach" {
			return nil
		}
		var success bool
		if successErr := json.Unmarshal(message["success"], &success); successErr != nil {
			return fmt.Errorf("decode DAP launch response success: %w", successErr)
		}
		if !success {
			return nil
		}
		return runtime.publishProcess(project, sessionID, nil)
	}
	if kind != "event" {
		return nil
	}
	if eventErr := json.Unmarshal(message["event"], &event); eventErr != nil {
		return fmt.Errorf("decode DAP event name: %w", eventErr)
	}
	if event != "process" {
		return nil
	}
	var body struct {
		Name  string `json:"name"`
		PID   *int64 `json:"systemProcessId"`
		Local *bool  `json:"isLocalProcess"`
	}
	if decodeErr := json.Unmarshal(message["body"], &body); decodeErr != nil {
		return fmt.Errorf("decode DAP process event: %w", decodeErr)
	}
	id, idErr := randdata.MakeRandomString(16)
	if idErr != nil {
		return fmt.Errorf("create process registration identity: %w", idErr)
	}
	entry := &apiv2.ProjectDebugProcessStatus{ID: string(id), Name: body.Name, PID: body.PID}
	var handle process.ProcessHandle
	switch {
	case body.Local != nil && !*body.Local:
		entry.TrackingError = "The adapter reported a non-local target; host process tracking and cleanup are unconfirmed."
	case body.PID == nil:
		entry.TrackingError = "The adapter did not report a target PID; tracking and cleanup are unconfirmed."
	default:
		pid, pidErr := process.Int64_ToPidT(*body.PID)
		if pidErr != nil {
			entry.TrackingError = fmt.Sprintf("Target PID is invalid; tracking and cleanup are unconfirmed: %v", pidErr)
		} else {
			captured, captureErr := runtime.executor.FindProcessHandle(pid)
			if captureErr != nil {
				entry.TrackingError = fmt.Sprintf("Target process identity is unavailable; tracking and cleanup are unconfirmed: %v", captureErr)
			} else {
				handle = captured
				entry.IdentityTimestamp = metav1.NewMicroTime(captured.IdentityTime)
			}
		}
	}
	if entry.TrackingError != "" {
		runtime.log.Info("Debug target tracking is unconfirmed", "Project", project.NamespacedName(), "Session", sessionID, "Reason", entry.TrackingError)
	}
	if publishErr := runtime.publishProcess(project, sessionID, entry); publishErr != nil {
		// If ownership cannot be handed off, roll back the captured target rather than silently losing it.
		if handle.Pid > 0 {
			cleanupCtx, cancel := process.WithDetachedStopTimeout(runtime.ctx)
			stopErr := runtime.executor.StopProcess(cleanupCtx, handle)
			cancel()
			if stopErr != nil && !process.IsProcessGoneErr(stopErr) {
				runtime.log.Error(stopErr, "Failed to clean up target after registration failure", "Project", project.NamespacedName(), "PID", handle.Pid)
				return errors.Join(publishErr, stopErr)
			}
		}
		return publishErr
	}
	return nil
}

func (runtime *projectDebugRuntime) publishProcess(project *apiv2.Project, sessionID string, entry *apiv2.ProjectDebugProcessStatus) error {
	ctx, cancel := process.WithDetachedStopTimeout(runtime.ctx)
	defer cancel()
	return resiliency.RetryExponential(ctx, func() error {
		_, updateErr := runtime.writer.registerProcess(ctx, project.NamespacedName(), project.UID, sessionID, entry)
		if apierrors.IsNotFound(updateErr) || apierrors.IsConflict(updateErr) || apierrors.IsForbidden(updateErr) {
			return resiliency.Permanent(updateErr)
		}
		return updateErr
	})
}
