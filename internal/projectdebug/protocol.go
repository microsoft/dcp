/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

func interruptProjectDebugRequests(session *apiv2.ProjectDebugSessionStatus) {
	for index := range session.ProtocolRequests {
		request := &session.ProtocolRequests[index]
		if request.Phase == apiv2.ProjectDebugRequestPending {
			request.Phase = apiv2.ProjectDebugRequestInterrupted
			request.FinishedAt = session.FinishedAt
			request.Message = "The debug session ended before a response was observed."
		}
	}
}

func (writer *projectDebugStatusWriter) observeRequest(
	ctx context.Context, name types.NamespacedName, uid types.UID, sessionID string, observation apiv2.ProjectDebugRequestStatus,
) (*apiv2.Project, error) {
	if observation.RequestSeq <= 0 ||
		(observation.Command != "initialize" && observation.Command != "launch" && observation.Command != "attach") {
		return nil, fmt.Errorf("DAP request sequence and supported command are required")
	}
	return writer.mutate(ctx, name, uid, func(project *apiv2.Project) error {
		if project.Status.Debug == nil {
			return fmt.Errorf("debug session %q is not registered", sessionID)
		}
		sessionIndex := slices.IndexFunc(project.Status.Debug.Sessions, func(entry apiv2.ProjectDebugSessionStatus) bool { return entry.ID == sessionID })
		if sessionIndex < 0 {
			return fmt.Errorf("debug session %q is not registered", sessionID)
		}
		session := &project.Status.Debug.Sessions[sessionIndex]
		index := slices.IndexFunc(session.ProtocolRequests, func(entry apiv2.ProjectDebugRequestStatus) bool { return entry.RequestSeq == observation.RequestSeq })
		if observation.Phase == apiv2.ProjectDebugRequestPending {
			if index < 0 {
				session.ProtocolRequests = append(session.ProtocolRequests, observation)
			} else if session.ProtocolRequests[index].Command != observation.Command {
				return fmt.Errorf("DAP request sequence %d was reused for a different command", observation.RequestSeq)
			}
			if session.Phase == apiv2.ProjectDebugPhaseExited || session.Phase == apiv2.ProjectDebugPhaseFailed {
				interruptProjectDebugRequests(session)
			}
		} else if index >= 0 {
			stored := &session.ProtocolRequests[index]
			if stored.Command == observation.Command && stored.Phase == apiv2.ProjectDebugRequestPending {
				*stored = observation
			}
		}
		return nil
	})
}

func (runtime *projectDebugRuntime) observeProtocolMessage(
	project *apiv2.Project, sessionID string, direction dap.Direction, message dap.Message,
) error {
	var kind, command string
	if typeErr := json.Unmarshal(message["type"], &kind); typeErr != nil {
		return fmt.Errorf("decode DAP protocol message type: %w", typeErr)
	}
	if (direction != dap.ClientToAdapter || kind != "request") && (direction != dap.AdapterToClient || kind != "response") {
		return nil
	}
	if commandErr := json.Unmarshal(message["command"], &command); commandErr != nil {
		return fmt.Errorf("decode DAP protocol command: %w", commandErr)
	}
	if command != "initialize" && command != "launch" && command != "attach" {
		return nil
	}
	observation := apiv2.ProjectDebugRequestStatus{Command: command, Phase: apiv2.ProjectDebugRequestPending}
	sequence := "seq"
	if kind == "response" {
		sequence = "request_seq"
		var success bool
		if successErr := json.Unmarshal(message["success"], &success); successErr != nil {
			return fmt.Errorf("decode DAP protocol response success: %w", successErr)
		}
		observation.Phase, observation.FinishedAt = apiv2.ProjectDebugRequestFailed, metav1.NowMicro()
		if success {
			observation.Phase = apiv2.ProjectDebugRequestSucceeded
		}
		if raw, present := message["message"]; present {
			if messageErr := json.Unmarshal(raw, &observation.Message); messageErr != nil {
				return fmt.Errorf("decode DAP protocol response message: %w", messageErr)
			}
		}
	}
	if sequenceErr := json.Unmarshal(message[sequence], &observation.RequestSeq); sequenceErr != nil {
		return fmt.Errorf("decode DAP protocol request sequence: %w", sequenceErr)
	}
	ctx, cancel := process.WithDetachedStopTimeout(runtime.ctx)
	defer cancel()
	return resiliency.RetryExponential(ctx, func() error {
		_, updateErr := runtime.writer.observeRequest(ctx, project.NamespacedName(), project.UID, sessionID, observation)
		if apierrors.IsNotFound(updateErr) || apierrors.IsConflict(updateErr) || apierrors.IsForbidden(updateErr) {
			return resiliency.Permanent(updateErr)
		}
		return updateErr
	})
}
