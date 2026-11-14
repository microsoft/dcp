/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/process"
)

type projectProcessData struct {
	observation              apiv2.ProjectDebugProcessObservation
	cleanupAttempted         bool
	deletionCleanupAttempted bool
	cleanupOwned             bool
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
			current.cleanupOwned = current.cleanupOwned || !registered.Attached
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
			if registered.Attached {
				observation.Cleanup = apiv2.ProjectDebugProcessCleanupNotOwned
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
	if data.processCleanupsPending != 0 {
		return false
	}
	var handles []process.ProcessHandle
	for handle, target := range data.processes {
		if !target.cleanupOwned {
			continue
		}
		if !target.cleanupAttempted || (!resource.DeletionTimestamp.IsZero() &&
			target.observation.Cleanup == apiv2.ProjectDebugProcessCleanupUnconfirmed && !target.deletionCleanupAttempted) {
			handles = append(handles, handle)
		}
	}
	if len(handles) == 0 {
		// Retain the handles until their cleanup observations survive any status-write conflict.
		return projectProcessObservations(resource.DeepCopy(), data)&statusChanged == 0
	}
	deleting := !resource.DeletionTimestamp.IsZero()
	data.processCleanupsPending = 1
	cleanupLog := r.Log.WithValues("Project", resource.NamespacedName(), "UID", resource.UID)
	enqueueErr := r.processCleanupQueue.Enqueue(func(ctx context.Context) {
		groups := groupProjectProcessCleanup(ctx, handles, cleanupLog)
		r.queueProjectProcessCleanupResult(resource, nil, len(groups)-1)
		for _, group := range groups {
			groupErr := r.processCleanupQueue.Enqueue(func(groupCtx context.Context) {
				results := r.cleanupProjectProcessGroup(groupCtx, group, deleting, cleanupLog)
				r.queueProjectProcessCleanupResult(resource, results, -1)
			})
			if groupErr != nil {
				cleanupLog.Error(groupErr, "Failed to queue Project target cleanup group")
				r.queueProjectProcessCleanupResult(resource, nil, -1)
			}
		}
	})
	if enqueueErr != nil {
		data.processCleanupsPending = 0
		log.Error(enqueueErr, "Failed to queue Project target cleanup")
	}
	return false
}

func groupProjectProcessCleanup(ctx context.Context, handles []process.ProcessHandle, log logr.Logger) [][]process.ProcessHandle {
	ordered := slices.Clone(handles)
	slices.SortFunc(ordered, func(left, right process.ProcessHandle) int {
		if order := left.IdentityTime.Compare(right.IdentityTime); order != 0 {
			return order
		}
		return cmp.Compare(left.Pid, right.Pid)
	})
	trees := make([][]process.ProcessHandle, len(ordered))
	uncertain := false
	for index, handle := range ordered {
		treeCtx, cancel := process.WithStopTimeout(ctx)
		tree, treeErr := process.GetProcessTree(treeCtx, handle)
		cancel()
		if treeErr != nil && (!process.IsProcessGoneErr(treeErr) || errors.Is(treeErr, process.ErrIncompleteProcessTree)) {
			log.Error(treeErr, "Could not establish independent Project target trees; cleaning targets sequentially", "PID", handle.Pid)
			uncertain = true
		}
		trees[index] = append(tree, handle)
	}
	ancestors := make(map[process.Pid_t]int)
	for _, handle := range ordered {
		ancestors[handle.Pid] = 0
	}
	for index, tree := range trees {
		for _, member := range tree {
			if _, registered := ancestors[member.Pid]; registered && member.Pid != ordered[index].Pid {
				ancestors[member.Pid]++
			}
		}
	}
	orderTargets := func(left, right process.ProcessHandle) int {
		if order := cmp.Compare(ancestors[left.Pid], ancestors[right.Pid]); order != 0 {
			return order
		}
		if order := left.IdentityTime.Compare(right.IdentityTime); order != 0 {
			return order
		}
		return cmp.Compare(left.Pid, right.Pid)
	}
	if uncertain {
		slices.SortFunc(ordered, orderTargets)
		return [][]process.ProcessHandle{ordered}
	}
	parents := make([]int, len(ordered))
	for index := range parents {
		parents[index] = index
	}
	root := func(index int) int {
		for parents[index] != index {
			parents[index] = parents[parents[index]]
			index = parents[index]
		}
		return index
	}
	owners := make(map[process.Pid_t]int)
	for index, tree := range trees {
		for _, member := range tree {
			if previous, found := owners[member.Pid]; found {
				parents[root(index)] = root(previous)
			} else {
				owners[member.Pid] = index
			}
		}
	}
	var groups [][]process.ProcessHandle
	groupIndexes := make(map[int]int)
	for index, handle := range ordered {
		groupRoot := root(index)
		groupIndex, found := groupIndexes[groupRoot]
		if !found {
			groupIndex = len(groups)
			groupIndexes[groupRoot] = groupIndex
			groups = append(groups, nil)
		}
		groups[groupIndex] = append(groups[groupIndex], handle)
	}
	for _, group := range groups {
		slices.SortFunc(group, orderTargets)
	}
	return groups
}

func (r *ProjectReconciler) cleanupProjectProcessGroup(
	ctx context.Context, handles []process.ProcessHandle, deleting bool, log logr.Logger,
) map[process.ProcessHandle]projectProcessData {
	results := make(map[process.ProcessHandle]projectProcessData, len(handles))
	for _, handle := range handles {
		cleanupCtx, cancel := process.WithStopTimeout(ctx)
		var stopErr error
		if !process.IsProcessGoneErr(r.processExecutor.CheckProcessRunning(handle)) {
			stopErr = r.processExecutor.StopProcess(cleanupCtx, handle)
		}
		cancel()
		result := projectProcessData{cleanupAttempted: true, deletionCleanupAttempted: deleting}
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
			log.Error(failure, "Project target cleanup is unconfirmed", "PID", handle.Pid)
		}
		results[handle] = result
	}
	return results
}

func (r *ProjectReconciler) queueProjectProcessCleanupResult(
	resource *apiv2.Project, results map[process.ProcessHandle]projectProcessData, pendingChange int,
) {
	queued := r.projects.QueueDeferredOpForStateKey(resource.NamespacedName(), resource.UID,
		func(name types.NamespacedName, uid types.UID, _ *apiv2.Project) {
			_, current := r.projects.BorrowByNamespacedName(name)
			if current == nil {
				return
			}
			for handle, result := range results {
				previous := current.processes[handle]
				if !previous.observation.FinishedAt.IsZero() {
					result.observation.FinishedAt = previous.observation.FinishedAt
				}
				current.processes[handle] = result
			}
			current.processCleanupsPending += pendingChange
			_ = r.projects.Update(name, uid, current)
		})
	if queued {
		r.ScheduleReconciliation(resource.NamespacedName())
	}
}
