/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/process"
)

func registeredProjectProcess(entry apiv2.ProjectDebugProcessStatus) (process.ProcessHandle, error) {
	if entry.ContainerID != "" || entry.TrackingError != "" || entry.PID == nil || entry.IdentityTimestamp.IsZero() {
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
		data.processes = make(map[process.ProcessHandle]apiv2.ProjectDebugProcessObservation)
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
				current.Phase, current.Message = apiv2.ProjectDebugProcessRunning, ""
				data.processPoll = true
			case process.IsProcessGoneErr(runningErr):
				current.Phase, current.Message = apiv2.ProjectDebugProcessExited, ""
				if current.FinishedAt.IsZero() {
					current.FinishedAt = metav1.NowMicro()
				}
			default:
				if current.Message != runningErr.Error() {
					log.Error(runningErr, "Failed to observe Project target process", "PID", handle.Pid)
				}
				current.Phase, current.Message = apiv2.ProjectDebugProcessUnknown, runningErr.Error()
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
				observation.Message = registered.TrackingError
				if observation.Message == "" {
					observation.Message = handleErr.Error()
				}
			} else if observed, found := data.processes[handle]; found {
				observation = observed
			} else {
				continue
			}
			change |= setValue(&registered.Observation.Phase, observation.Phase)
			change |= setTimestamp(&registered.Observation.FinishedAt, observation.FinishedAt)
			change |= setValue(&registered.Observation.Message, observation.Message)
		}
	}
	return change
}
