/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/dap"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

func (writer *projectDebugStatusWriter) updateProgress(ctx context.Context, name types.NamespacedName, uid types.UID,
	sessionID string, progress *apiv2.ProjectDebugSessionProgress,
) (*apiv2.Project, error) {
	snapshot := progress.DeepCopy()
	return writer.mutate(ctx, name, uid, func(project *apiv2.Project) error {
		if project.Status.Debug == nil {
			return fmt.Errorf("debug session %q is not registered", sessionID)
		}
		index := slices.IndexFunc(project.Status.Debug.Sessions, func(session apiv2.ProjectDebugSessionStatus) bool {
			return session.ID == sessionID
		})
		if index < 0 {
			return fmt.Errorf("debug session %q is not registered", sessionID)
		}
		session := &project.Status.Debug.Sessions[index]
		session.Progress = snapshot.DeepCopy()
		applyProjectDebugSessionProgress(session, project.Generation)
		return nil
	})
}

func applyProjectDebugSessionProgress(session *apiv2.ProjectDebugSessionStatus, generation int64) {
	if session.Progress == nil || session.Phase == apiv2.ProjectDebugPhaseExited || session.Phase == apiv2.ProjectDebugPhaseFailed {
		return
	}
	progress := session.Progress
	ready := progress.Phase == apiv2.ProjectDebugConnectionConnected &&
		progress.InitializeRequested && progress.AdapterInitialized
	reason, message := "DAPInitializationPending", "Both streams are connected; waiting for DAP initialization."
	switch progress.Phase {
	case apiv2.ProjectDebugConnectionTerminating:
		reason, message = "SessionTerminating", "Debug session termination has started."
	case apiv2.ProjectDebugConnectionDisconnected:
		reason, message = "StreamDisconnected", "A debug stream has disconnected."
	default:
		if ready {
			reason, message = "DAPInitialized", "Client initialization and adapter initialized event have been observed."
		}
	}
	session.Conditions = debugConditions(reason, message, ready, generation)
	if progress.Message != "" {
		session.Conditions[0].Message = progress.Message
	}
}

func (runtime *projectDebugRuntime) publishProgress(project *apiv2.Project, sessionID string, state dap.SessionState) error {
	var phase apiv2.ProjectDebugConnectionPhase
	switch state.Phase {
	case dap.ConnectionConnected:
		phase = apiv2.ProjectDebugConnectionConnected
	case dap.ConnectionTerminating:
		phase = apiv2.ProjectDebugConnectionTerminating
	case dap.ConnectionDisconnected:
		phase = apiv2.ProjectDebugConnectionDisconnected
	default:
		return fmt.Errorf("invalid bridge connection phase %d", state.Phase)
	}
	progress := &apiv2.ProjectDebugSessionProgress{
		Phase: phase, StreamsConnected: state.StreamsConnected, InitializeRequested: state.InitializeRequested,
		LaunchRequested: state.LaunchRequested, AttachRequested: state.AttachRequested, AdapterInitialized: state.AdapterInitialized,
		ProcessReported: state.ProcessReported, ClientTerminating: state.ClientTerminating, AdapterTerminating: state.AdapterTerminating,
		ResourceTerminating: state.ResourceTerminating, DebuggeeExited: state.DebuggeeExited, DebuggeeExitCode: state.DebuggeeExitCode,
		ClientDisconnected: state.ClientDisconnected, AdapterDisconnected: state.AdapterDisconnected,
		ClientDisconnectMessage: state.ClientDisconnectMessage, AdapterDisconnectMessage: state.AdapterDisconnectMessage, Message: state.Message,
	}
	ctx, cancel := process.WithDetachedStopTimeout(runtime.ctx)
	defer cancel()
	return resiliency.RetryExponential(ctx, func() error {
		_, updateErr := runtime.writer.updateProgress(ctx, project.NamespacedName(), project.UID, sessionID, progress)
		if apierrors.IsNotFound(updateErr) || apierrors.IsConflict(updateErr) || apierrors.IsForbidden(updateErr) {
			return resiliency.Permanent(updateErr)
		}
		return updateErr
	})
}
