/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/process"
)

type projectProcessData struct {
	observation      apiv2.ProjectDebugProcessObservation
	cleanupAttempted bool
}

type projectProcessCleanupOperation struct {
	handles []process.ProcessHandle
}

func logUnconfirmedProjectTargets(resource *apiv2.Project, log logr.Logger) {
	if resource.Status.Debug == nil {
		return
	}
	for _, session := range resource.Status.Debug.Sessions {
		if session.ProcessTrackingUnconfirmed {
			log.Info("Project target tracking and cleanup are unconfirmed", "Session", session.ID, "Reason", session.ProcessTrackingMessage)
		}
	}
}

func registeredProjectProcess(entry apiv2.ProjectDebugProcessStatus) (process.ProcessHandle, error) {
	if entry.TrackingError != "" || entry.PID == nil || entry.IdentityTimestamp.IsZero() {
		return process.ProcessHandle{}, fmt.Errorf("local process identity is unconfirmed")
	}
	pid, pidErr := process.Int64_ToPidT(*entry.PID)
	if pidErr != nil {
		return process.ProcessHandle{}, pidErr
	}
	handle := process.NewHandle(pid, entry.IdentityTimestamp.Time)
	return handle, handle.Validate()
}

func (r *ProjectReconciler) observeProjectProcesses(resource *apiv2.Project, data *projectData, log logr.Logger) {
	data.processPoll = false
	if data.processes == nil {
		data.processes = make(map[process.ProcessHandle]projectProcessData)
	}
	if resource.Status.Debug == nil {
		return
	}
	for _, session := range resource.Status.Debug.Sessions {
		for _, registered := range session.Processes {
			handle, handleErr := registeredProjectProcess(registered)
			if handleErr != nil {
				continue
			}
			current := data.processes[handle]
			runningErr := r.processExecutor.CheckProcessRunning(handle)
			switch {
			case runningErr == nil:
				current.observation.Phase = apiv2.ProjectDebugProcessRunning
				data.processPoll = true
				if current.observation.Cleanup != apiv2.ProjectDebugProcessCleanupUnconfirmed {
					current.observation.Message = ""
				}
			case process.IsProcessGoneErr(runningErr):
				current.observation.Phase = apiv2.ProjectDebugProcessExited
				if current.observation.FinishedAt.IsZero() {
					current.observation.FinishedAt = metav1.NowMicro()
				}
			default:
				if current.observation.Message != runningErr.Error() {
					log.Error(runningErr, "Failed to observe Project target process", "PID", handle.Pid)
				}
				current.observation.Phase, current.observation.Message = apiv2.ProjectDebugProcessUnknown, runningErr.Error()
				data.processPoll = true
			}
			data.processes[handle] = current
		}
	}
}

func projectProcessObservations(resource *apiv2.Project, data *projectData) objectChange {
	if resource.Status.Debug == nil {
		return noChange
	}
	change := noChange
	for sessionIndex := range resource.Status.Debug.Sessions {
		session := &resource.Status.Debug.Sessions[sessionIndex]
		for processIndex := range session.Processes {
			registered := &session.Processes[processIndex]
			handle, handleErr := registeredProjectProcess(*registered)
			observation := apiv2.ProjectDebugProcessObservation{}
			if handleErr != nil {
				observation.Phase = apiv2.ProjectDebugProcessUnknown
				observation.Cleanup = apiv2.ProjectDebugProcessCleanupUnconfirmed
				observation.Message = registered.TrackingError
				if observation.Message == "" {
					observation.Message = handleErr.Error()
				}
			} else if observed, found := data.processes[handle]; found {
				observation = observed.observation
			} else {
				continue
			}
			change |= setValue(&registered.Observation.Phase, observation.Phase)
			change |= setTimestamp(&registered.Observation.FinishedAt, observation.FinishedAt)
			change |= setValue(&registered.Observation.Cleanup, observation.Cleanup)
			change |= setValue(&registered.Observation.Message, observation.Message)
		}
	}
	return change
}

func (r *ProjectReconciler) settleProjectProcesses(resource *apiv2.Project, data *projectData, log logr.Logger) bool {
	r.observeProjectProcesses(resource, data, log)
	if data.processCleanup != nil {
		return false
	}
	var handles []process.ProcessHandle
	for handle, target := range data.processes {
		if !target.cleanupAttempted {
			handles = append(handles, handle)
		}
	}
	if len(handles) == 0 {
		// Retain the handles until their cleanup observations survive any status-write conflict.
		return projectProcessObservations(resource.DeepCopy(), data)&statusChanged == 0
	}
	operation := &projectProcessCleanupOperation{handles: handles}
	data.processCleanup = operation
	cleanupLog := r.Log.WithValues("Project", resource.NamespacedName(), "UID", resource.UID)
	enqueueErr := r.processCleanupQueue.Enqueue(func(ctx context.Context) {
		cleanupCtx, cancel := process.WithStopTimeout(ctx)
		defer cancel()
		results := make(map[process.ProcessHandle]projectProcessData, len(operation.handles))
		for _, handle := range operation.handles {
			stopErr := r.processExecutor.StopProcess(cleanupCtx, handle)
			result := projectProcessData{cleanupAttempted: true}
			runningErr := r.processExecutor.CheckProcessRunning(handle)
			switch {
			case process.IsProcessGoneErr(runningErr):
				result.observation.Phase = apiv2.ProjectDebugProcessExited
				result.observation.FinishedAt = metav1.NowMicro()
			case runningErr == nil:
				result.observation.Phase = apiv2.ProjectDebugProcessRunning
			default:
				result.observation.Phase = apiv2.ProjectDebugProcessUnknown
			}
			if (stopErr == nil || (process.IsProcessGoneErr(stopErr) && !errors.Is(stopErr, process.ErrIncompleteProcessTree))) &&
				process.IsProcessGoneErr(runningErr) {
				result.observation.Cleanup = apiv2.ProjectDebugProcessCleanupComplete
			} else {
				failure := errors.Join(stopErr, runningErr, fmt.Errorf("target process cleanup could not be fully confirmed"))
				result.observation.Cleanup = apiv2.ProjectDebugProcessCleanupUnconfirmed
				result.observation.Message = failure.Error()
				cleanupLog.Error(failure, "Project target cleanup is unconfirmed", "PID", handle.Pid)
			}
			results[handle] = result
		}
		queued := r.projects.QueueDeferredOpForStateKey(resource.NamespacedName(), resource.UID,
			func(name types.NamespacedName, uid types.UID, _ *apiv2.Project) {
				_, current := r.projects.BorrowByNamespacedName(name)
				if current == nil || current.processCleanup != operation {
					return
				}
				for handle, result := range results {
					previous := current.processes[handle]
					if !previous.observation.FinishedAt.IsZero() {
						result.observation.FinishedAt = previous.observation.FinishedAt
					}
					current.processes[handle] = result
				}
				current.processCleanup = nil
				_ = r.projects.Update(name, uid, current)
			})
		if queued {
			r.ScheduleReconciliation(resource.NamespacedName())
		}
	})
	if enqueueErr != nil {
		data.processCleanup = nil
		log.Error(enqueueErr, "Failed to queue Project target cleanup")
		data.retry("ProcessCleanupQueueFailed", enqueueErr)
	}
	return false
}
