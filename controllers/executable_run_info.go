// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"fmt"
	stdlib_maps "maps"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

// Stores information about Executable run
type ExecutableRunInfo struct {
	// State of the run (starting, running, finished, etc.)
	ExeState apiv1.ExecutableState

	// Process ID of the process that runs the Executable
	Pid *int64

	// Run ID for the Executable
	RunID RunID

	// Exit code of the Executable process
	ExitCode *int32

	// Timestamp when the run was started
	StartupTimestamp metav1.MicroTime

	// Timestamp when the run was finished
	FinishTimestamp metav1.MicroTime

	// Paths to captured standard output and standard error files
	StdOutFile string
	StdErrFile string

	// The map of ports reserved for services that the Executable implements
	ReservedPorts map[types.NamespacedName]int32

	// The most recent health probe results (indexed by probe name)
	healthProbeResults map[string]apiv1.HealthProbeResult

	// Whether health probes are enabled for the Executable
	healthProbesEnabled *bool
}

func NewRunInfo() *ExecutableRunInfo {
	return &ExecutableRunInfo{
		ExeState:           "",
		Pid:                apiv1.UnknownPID,
		ExitCode:           apiv1.UnknownExitCode,
		StartupTimestamp:   metav1.MicroTime{},
		FinishTimestamp:    metav1.MicroTime{},
		healthProbeResults: make(map[string]apiv1.HealthProbeResult),
	}
}

// Updates the runInfo object from another instance that supplies new data about the run.
// The object that is updated represents the "last known good" state of the run.
// Since change notifications about real-world counterparts of the Executable object
// come asynchronously and may come out of order, we do not take all updates blindly.
// For example, we restrict the state transitions to only the valid ones.
func (ri *ExecutableRunInfo) UpdateFrom(updated *ExecutableRunInfo, log logr.Logger) {
	if updated == nil {
		return
	}

	if ri.ExeState != updated.ExeState {
		if ri.ExeState.CanUpdateTo(updated.ExeState) {
			ri.ExeState = updated.ExeState
		} else {
			log.V(1).Info("ignoring invalid Executable state transition", "CurrentRunInfo", ri.String(), "UpdatedRunInfo", updated.String())
			return // Do not update other fields if the state transition is invalid
		}
	}

	if updated.Pid != apiv1.UnknownPID {
		ri.Pid = new(int64)
		*ri.Pid = *updated.Pid
	} else if updated.ExeState.IsTerminal() {
		ri.Pid = apiv1.UnknownPID
	}

	if updated.RunID != "" {
		ri.RunID = updated.RunID
	} else if updated.ExeState.IsTerminal() {
		ri.RunID = ""
	}

	if updated.ExitCode != apiv1.UnknownExitCode {
		ri.ExitCode = new(int32)
		*ri.ExitCode = *updated.ExitCode
	}

	if !updated.StartupTimestamp.IsZero() && (ri.StartupTimestamp.IsZero() || updated.StartupTimestamp.Before(&ri.StartupTimestamp)) {
		ri.StartupTimestamp = updated.StartupTimestamp
	}

	if !updated.FinishTimestamp.IsZero() && (ri.FinishTimestamp.IsZero() || updated.FinishTimestamp.Before(&ri.FinishTimestamp)) {
		ri.FinishTimestamp = updated.FinishTimestamp
	}

	if updated.StdOutFile != "" {
		ri.StdOutFile = updated.StdOutFile
	}

	if updated.StdErrFile != "" {
		ri.StdErrFile = updated.StdErrFile
	}

	if len(updated.ReservedPorts) > 0 {
		stdlib_maps.Insert(ri.ReservedPorts, stdlib_maps.All(updated.ReservedPorts))
	}

	if len(updated.healthProbeResults) > 0 {
		stdlib_maps.Insert(ri.healthProbeResults, stdlib_maps.All(updated.healthProbeResults))
	}

	if updated.healthProbesEnabled != nil {
		ri.healthProbesEnabled = new(bool)
		*ri.healthProbesEnabled = *updated.healthProbesEnabled
	}
}

func (ri *ExecutableRunInfo) DeepCopy() *ExecutableRunInfo {
	retval := ExecutableRunInfo{
		ExeState: ri.ExeState,
	}
	if ri.Pid != apiv1.UnknownPID {
		retval.Pid = new(int64)
		*retval.Pid = *ri.Pid
	}
	retval.RunID = ri.RunID
	if ri.ExitCode != apiv1.UnknownExitCode {
		retval.ExitCode = new(int32)
		*retval.ExitCode = *ri.ExitCode
	}
	if len(ri.ReservedPorts) > 0 {
		retval.ReservedPorts = stdlib_maps.Clone(ri.ReservedPorts)
	}
	retval.StartupTimestamp = ri.StartupTimestamp
	retval.FinishTimestamp = ri.FinishTimestamp
	retval.StdOutFile = ri.StdOutFile
	retval.StdErrFile = ri.StdErrFile
	retval.healthProbeResults = stdlib_maps.Clone(ri.healthProbeResults)
	if ri.healthProbesEnabled != nil {
		retval.healthProbesEnabled = new(bool)
		*retval.healthProbesEnabled = *ri.healthProbesEnabled
	}
	return &retval
}

func (ri *ExecutableRunInfo) ApplyTo(exe *apiv1.Executable, log logr.Logger) objectChange {
	status := &exe.Status
	changed := noChange

	if ri.ExeState != "" && status.State != ri.ExeState {
		status.State = ri.ExeState
		changed = statusChanged
	}

	if ri.Pid != apiv1.UnknownPID && (status.PID == apiv1.UnknownPID || *status.PID != *ri.Pid) {
		status.PID = new(int64)
		*status.PID = *ri.Pid
		changed = statusChanged
	}

	if ri.RunID != "" && status.ExecutionID != string(ri.RunID) {
		status.ExecutionID = string(ri.RunID)
		changed = statusChanged
	}

	if ri.ExitCode != apiv1.UnknownExitCode && (status.ExitCode == nil || *status.ExitCode != *ri.ExitCode) {
		status.ExitCode = new(int32)
		*status.ExitCode = *ri.ExitCode
		changed = statusChanged
	}

	// We only overwrite timestamps if the Executable status has them as zero values
	// or if the run info indicates that the startup/finish happened earlier.
	// The controller provides a default value for timestamps based on Executable state transitions,
	// but a more accurate timestamp may come (asynchronously) from Executable runner via run info.

	if !ri.StartupTimestamp.IsZero() && (status.StartupTimestamp.IsZero() || ri.StartupTimestamp.Before(&status.StartupTimestamp)) {
		status.StartupTimestamp = ri.StartupTimestamp
		changed = statusChanged
	}

	if !ri.FinishTimestamp.IsZero() && (status.FinishTimestamp.IsZero() || ri.FinishTimestamp.Before(&status.FinishTimestamp)) {
		status.FinishTimestamp = ri.FinishTimestamp
		changed = statusChanged
	}

	if ri.StdOutFile != "" && status.StdOutFile != ri.StdOutFile {
		status.StdOutFile = ri.StdOutFile
		changed = statusChanged
	}

	if ri.StdErrFile != "" && status.StdErrFile != ri.StdErrFile {
		status.StdErrFile = ri.StdErrFile
		changed = statusChanged
	}

	healthResultsChanged := false
	statusResults := maps.SliceToMap(status.HealthProbeResults, func(hpr apiv1.HealthProbeResult) (string, apiv1.HealthProbeResult) {
		return hpr.ProbeName, hpr
	})
	if len(statusResults) == 0 && len(ri.healthProbeResults) > 0 {
		statusResults = make(map[string]apiv1.HealthProbeResult)
	}
	for probeName, hpr := range ri.healthProbeResults {
		statusHpr, found := statusResults[probeName]
		if !found {
			statusResults[probeName] = hpr
			healthResultsChanged = true
		} else {
			res, timestampDiff := hpr.Diff(&statusHpr)
			timestampDiff = timestampDiff.Abs()
			needsUpdate := res == apiv1.Different || (res == apiv1.DiffTimestampOnly && timestampDiff >= maxStaleHealthProbeResultAge)
			if needsUpdate {
				statusResults[probeName] = hpr
				healthResultsChanged = true
			}
		}
	}
	if healthResultsChanged {
		status.HealthProbeResults = maps.Values(statusResults)
		changed = statusChanged
	}

	changed |= updateExecutableHealthStatus(exe, ri.ExeState, log)

	if changed != noChange && log.V(1).Enabled() {
		log.V(1).Info("Executable run changed", "CurrentRunInfo", ri.String(), "Executable", exe.NamespacedName().String())
	}

	return changed
}

func (ri *ExecutableRunInfo) String() string {
	return fmt.Sprintf(
		"{exeState=%s, pid=%s, executionID=%s, exitCode=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s, healthProbeResults=%s, healthProbesEnabled=%s}",
		ri.ExeState,
		logger.IntPtrValToString(ri.Pid),
		logger.FriendlyString(string(ri.RunID)),
		logger.IntPtrValToString(ri.ExitCode),
		logger.FriendlyMetav1Timestamp(ri.StartupTimestamp),
		logger.FriendlyMetav1Timestamp(ri.FinishTimestamp),
		logger.FriendlyString(ri.StdOutFile),
		logger.FriendlyString(ri.StdErrFile),
		ri.healthProbesFriendlyString(),
		logger.BoolPtrValToString(ri.healthProbesEnabled),
	)
}

func DiffString(r1, r2 *ExecutableRunInfo) string {
	sb := strings.Builder{}
	sb.WriteString("{")

	if r1.ExeState != r2.ExeState {
		sb.WriteString(fmt.Sprintf("exeState=%s->%s, ", r1.ExeState, r2.ExeState))
	}

	if !pointers.EqualValue(r1.Pid, r2.Pid) {
		sb.WriteString(fmt.Sprintf("pid=%s->%s, ", logger.IntPtrValToString(r1.Pid), logger.IntPtrValToString(r2.Pid)))
	}

	if r1.RunID != r2.RunID {
		sb.WriteString(fmt.Sprintf("executionID=%s->%s, ", logger.FriendlyString(string(r1.RunID)), logger.FriendlyString(string(r2.RunID))))
	}

	if !pointers.EqualValue(r1.ExitCode, r2.ExitCode) {
		sb.WriteString(fmt.Sprintf("exitCode=%s->%s, ", logger.IntPtrValToString(r1.ExitCode), logger.IntPtrValToString(r2.ExitCode)))
	}

	if !r1.StartupTimestamp.Equal(&r2.StartupTimestamp) {
		sb.WriteString(fmt.Sprintf("startupTimestamp=%s->%s, ", logger.FriendlyMetav1Timestamp(r1.StartupTimestamp), logger.FriendlyMetav1Timestamp(r2.StartupTimestamp)))
	}

	if !r1.FinishTimestamp.Equal(&r2.FinishTimestamp) {
		sb.WriteString(fmt.Sprintf("finishTimestamp=%s->%s, ", logger.FriendlyMetav1Timestamp(r1.FinishTimestamp), logger.FriendlyMetav1Timestamp(r2.FinishTimestamp)))
	}

	if r1.StdOutFile != r2.StdOutFile {
		sb.WriteString(fmt.Sprintf("stdOutFile=%s->%s, ", logger.FriendlyString(r1.StdOutFile), logger.FriendlyString(r2.StdOutFile)))
	}

	if r1.StdErrFile != r2.StdErrFile {
		sb.WriteString(fmt.Sprintf("stdErrFile=%s->%s, ", logger.FriendlyString(r1.StdErrFile), logger.FriendlyString(r2.StdErrFile)))
	}

	if !stdlib_maps.Equal(r1.healthProbeResults, r2.healthProbeResults) {
		sb.WriteString(fmt.Sprintf("healthProbeResults=%s->%s, ", r1.healthProbesFriendlyString(), r2.healthProbesFriendlyString()))
	}

	if !pointers.EqualValue(r1.healthProbesEnabled, r2.healthProbesEnabled) {
		sb.WriteString(fmt.Sprintf("healthProbesEnabled=%s->%s, ", logger.BoolPtrValToString(r1.healthProbesEnabled), logger.BoolPtrValToString(r2.healthProbesEnabled)))
	}

	sb.WriteString("}")
	return sb.String()
}

func (ri *ExecutableRunInfo) healthProbesFriendlyString() string {
	return logger.FriendlyStringSlice(maps.MapToSlice[string](ri.healthProbeResults, func(v apiv1.HealthProbeResult) string { return v.String() }))
}

var _ fmt.Stringer = (*ExecutableRunInfo)(nil)
