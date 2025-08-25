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
	"github.com/microsoft/usvc-apiserver/internal/health"
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

	// UID of the Executable
	UID types.UID

	// Exit code of the Executable process
	ExitCode *int32

	// Timestamp when the run was started
	StartupTimestamp metav1.MicroTime

	// Timestamp when the run was finished
	FinishTimestamp metav1.MicroTime

	// Paths to captured standard output and standard error files
	StdOutFile string
	StdErrFile string

	// Paths to capture debug, info, and error logs
	DebugLogFile string
	InfoLogFile  string
	ErrorLogFile string

	// The map of ports reserved for services that the Executable implements
	ReservedPorts map[types.NamespacedName]int32

	// The most recent health probe results (indexed by probe name)
	healthProbeResults map[string]apiv1.HealthProbeResult

	// Whether health probes are enabled for the Executable
	healthProbesEnabled *bool

	// True if Executable stop was initiated/queued.
	stopAttemptInitiated bool
}

func NewRunInfo(exe *apiv1.Executable) *ExecutableRunInfo {
	return &ExecutableRunInfo{
		ExeState:           "",
		Pid:                apiv1.UnknownPID,
		UID:                exe.UID,
		ExitCode:           apiv1.UnknownExitCode,
		StartupTimestamp:   metav1.MicroTime{},
		FinishTimestamp:    metav1.MicroTime{},
		healthProbeResults: make(map[string]apiv1.HealthProbeResult),
	}
}

func (ri *ExecutableRunInfo) GetResourceId() string {
	return fmt.Sprintf("executable-%s", ri.UID)
}

// Updates the runInfo object from another instance that supplies new data about the run.
// Returns true if any changes were made, false otherwise.
//
// The object that is updated may represent the "last known good" state of the run.
// Since change notifications about real-world counterparts of the Executable object
// come asynchronously and may come out of order, we do not take all updates blindly.
// For example, we restrict the state transitions to only the valid ones.
func (ri *ExecutableRunInfo) UpdateFrom(other *ExecutableRunInfo) bool {
	if other == nil {
		return false
	}

	updated := false

	if ri.ExeState != other.ExeState {
		if ri.ExeState.CanUpdateTo(other.ExeState) {
			ri.ExeState = other.ExeState
			updated = true
		} else {
			return false // Do not update other fields if the state transition is invalid
		}
	}

	if other.Pid != apiv1.UnknownPID && !pointers.EqualValue(ri.Pid, other.Pid) {
		pointers.SetValueFrom(&ri.Pid, other.Pid)
		updated = true
	} else if other.ExeState.IsTerminal() && ri.Pid != apiv1.UnknownPID {
		ri.Pid = apiv1.UnknownPID
		updated = true
	}

	if other.RunID != "" && ri.RunID != other.RunID {
		ri.RunID = other.RunID
		updated = true
	} else if other.ExeState.IsTerminal() && ri.RunID != "" {
		ri.RunID = ""
		updated = true
	}

	if other.UID != "" && ri.UID != other.UID {
		ri.UID = other.UID
		updated = true
	}

	if other.ExitCode != apiv1.UnknownExitCode && !pointers.EqualValue(ri.ExitCode, other.ExitCode) {
		pointers.SetValueFrom(&ri.ExitCode, other.ExitCode)
		updated = true
	}

	updated = setTimestampIfAfterOrUnknown(other.StartupTimestamp, &ri.StartupTimestamp) || updated
	updated = setTimestampIfAfterOrUnknown(other.FinishTimestamp, &ri.FinishTimestamp) || updated

	if other.StdOutFile != "" && ri.StdOutFile != other.StdOutFile {
		ri.StdOutFile = other.StdOutFile
		updated = true
	}

	if other.StdErrFile != "" && ri.StdErrFile != other.StdErrFile {
		ri.StdErrFile = other.StdErrFile
		updated = true
	}

	if other.DebugLogFile != "" && ri.DebugLogFile != other.DebugLogFile {
		ri.DebugLogFile = other.DebugLogFile
		updated = true
	}

	if other.InfoLogFile != "" && ri.InfoLogFile != other.InfoLogFile {
		ri.InfoLogFile = other.InfoLogFile
		updated = true
	}

	if other.ErrorLogFile != "" && ri.ErrorLogFile != other.ErrorLogFile {
		ri.ErrorLogFile = other.ErrorLogFile
		updated = true
	}

	if len(other.ReservedPorts) > 0 {
		updated = maps.Insert(ri.ReservedPorts, stdlib_maps.All(other.ReservedPorts)) || updated
	}

	if len(other.healthProbeResults) > 0 {
		updated = maps.Insert(ri.healthProbeResults, stdlib_maps.All(other.healthProbeResults)) || updated
	}

	if other.healthProbesEnabled != nil && !pointers.EqualValue(ri.healthProbesEnabled, other.healthProbesEnabled) {
		pointers.SetValueFrom(&ri.healthProbesEnabled, other.healthProbesEnabled)
		updated = true
	}

	return updated
}

func (ri *ExecutableRunInfo) Clone() *ExecutableRunInfo {
	retval := ExecutableRunInfo{
		ExeState: ri.ExeState,
	}
	if ri.Pid != apiv1.UnknownPID {
		pointers.SetValueFrom(&retval.Pid, ri.Pid)
	}
	retval.RunID = ri.RunID
	retval.UID = ri.UID
	if ri.ExitCode != apiv1.UnknownExitCode {
		pointers.SetValueFrom(&retval.ExitCode, ri.ExitCode)
	}
	if len(ri.ReservedPorts) > 0 {
		retval.ReservedPorts = stdlib_maps.Clone(ri.ReservedPorts)
	}
	retval.StartupTimestamp = ri.StartupTimestamp
	retval.FinishTimestamp = ri.FinishTimestamp
	retval.StdOutFile = ri.StdOutFile
	retval.StdErrFile = ri.StdErrFile
	retval.DebugLogFile = ri.DebugLogFile
	retval.InfoLogFile = ri.InfoLogFile
	retval.ErrorLogFile = ri.ErrorLogFile
	retval.healthProbeResults = stdlib_maps.Clone(ri.healthProbeResults)
	pointers.SetValueFrom(&retval.healthProbesEnabled, ri.healthProbesEnabled)
	retval.stopAttemptInitiated = ri.stopAttemptInitiated
	return &retval
}

func (ri *ExecutableRunInfo) ApplyTo(exe *apiv1.Executable, log logr.Logger) objectChange {
	status := &exe.Status
	changed := noChange

	if ri.ExeState != apiv1.ExecutableStateEmpty && status.State != ri.ExeState {
		status.State = ri.ExeState
		changed = statusChanged
	}

	if ri.Pid != apiv1.UnknownPID && (status.PID == apiv1.UnknownPID || *status.PID != *ri.Pid) {
		pointers.SetValueFrom(&status.PID, ri.Pid)
		changed = statusChanged
	}

	if ri.RunID != "" && status.ExecutionID != string(ri.RunID) {
		status.ExecutionID = string(ri.RunID)
		changed = statusChanged
	}

	if ri.ExitCode != apiv1.UnknownExitCode && (status.ExitCode == nil || *status.ExitCode != *ri.ExitCode) {
		pointers.SetValueFrom(&status.ExitCode, ri.ExitCode)
		changed = statusChanged
	}

	// We only overwrite timestamps if the Executable status has them as zero values
	// or if the run info indicates that the startup/finish happened earlier.
	// The controller provides a default value for timestamps based on Executable state transitions,
	// but a more accurate (earlier) timestamp may come (asynchronously) from Executable runner via run info.

	if setTimestampIfAfterOrUnknown(ri.StartupTimestamp, &status.StartupTimestamp) {
		changed = statusChanged
	}

	if setTimestampIfAfterOrUnknown(ri.FinishTimestamp, &status.FinishTimestamp) {
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

	updatedHealthResults, healthResultsChanged := health.UpdateHealthProbeResults(status.HealthProbeResults, ri.healthProbeResults)
	if healthResultsChanged {
		status.HealthProbeResults = updatedHealthResults
		changed = statusChanged
	}

	changed |= updateExecutableHealthStatus(exe, ri.ExeState, log)

	if changed != noChange && log.V(1).Enabled() {
		log.V(1).Info("Executable run changed", "CurrentRunInfo", ri.String())
	}

	return changed
}

func (ri *ExecutableRunInfo) String() string {
	return fmt.Sprintf(
		"{exeState=%s, pid=%s, executionID=%s, exitCode=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s, healthProbeResults=%s, healthProbesEnabled=%s, stopAttemptInitiated=%t}",
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
		ri.stopAttemptInitiated,
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

	if r1.stopAttemptInitiated != r2.stopAttemptInitiated {
		sb.WriteString(fmt.Sprintf("stopAttemptInitiated=%t->%t, ", r1.stopAttemptInitiated, r2.stopAttemptInitiated))
	}

	sb.WriteString("}")
	return sb.String()
}

func (ri *ExecutableRunInfo) healthProbesFriendlyString() string {
	return logger.FriendlyStringSlice(maps.MapToSlice[string](ri.healthProbeResults, func(v apiv1.HealthProbeResult) string { return v.String() }))
}

var _ fmt.Stringer = (*ExecutableRunInfo)(nil)
var _ Cloner[*ExecutableRunInfo] = (*ExecutableRunInfo)(nil)
var _ UpdateableFrom[*ExecutableRunInfo] = (*ExecutableRunInfo)(nil)
