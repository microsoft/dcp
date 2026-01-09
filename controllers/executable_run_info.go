/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"fmt"
	stdlib_maps "maps"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/health"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/slices"
)

// The startup stage for an Executable run. Non-negative values
// indicate that the startup progressed beyond initialization stage
// and we are using the Executable runner with index equal to the stage value
// (0 for default runner, 1 for first fallback, etc.).
type ExecutableStartuptStage int

const (
	StartupStageInitial              ExecutableStartuptStage = -3
	StartupStageCertificateDataReady ExecutableStartuptStage = -2
	StartupStageDataInitialized      ExecutableStartuptStage = -1
	StartupStageDefaultRunner        ExecutableStartuptStage = 0
)

func (s ExecutableStartuptStage) String() string {
	switch s {
	case StartupStageInitial:
		return "initial"
	case StartupStageCertificateDataReady:
		return "certificate data ready"
	case StartupStageDataInitialized:
		return "run data initialized"
	case StartupStageDefaultRunner:
		return "using default runner"
	default:
		if s > 0 {
			return fmt.Sprintf("using fallback runner %d", int(s)-1)
		} else {
			return fmt.Sprintf("unknown negative stage %d", int(s)) // Should never happen
		}
	}
}

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

	// The startup stage reached for the Executable
	startupStage ExecutableStartuptStage

	// Results of Executable startup attempts
	startResults []*ExecutableStartResult
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
		startupStage:       StartupStageInitial,
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
		if ri.ReservedPorts == nil {
			ri.ReservedPorts = make(map[types.NamespacedName]int32)
		}
		updated = maps.Insert(ri.ReservedPorts, stdlib_maps.All(other.ReservedPorts)) || updated
	}

	if len(other.healthProbeResults) > 0 {
		updated = maps.Insert(ri.healthProbeResults, stdlib_maps.All(other.healthProbeResults)) || updated
	}

	if other.healthProbesEnabled != nil && !pointers.EqualValue(ri.healthProbesEnabled, other.healthProbesEnabled) {
		pointers.SetValueFrom(&ri.healthProbesEnabled, other.healthProbesEnabled)
		updated = true
	}

	if other.startupStage != StartupStageInitial && ri.startupStage != other.startupStage {
		ri.startupStage = other.startupStage
		updated = true
	}

	if len(other.startResults) >= len(ri.startResults) {
		different := len(other.startResults) > len(ri.startResults) || slices.Any(ri.startResults, func(i int, v *ExecutableStartResult) bool {
			return !v.Equal(other.startResults[i])
		})

		if different {
			// ExecutableStartResult contains pointers, so clone to avoid false sharing.
			ri.startResults = slices.Map[*ExecutableStartResult](other.startResults, (*ExecutableStartResult).Clone)
			updated = true
		}
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
	retval.startupStage = ri.startupStage
	retval.startResults = slices.Map[*ExecutableStartResult](ri.startResults, (*ExecutableStartResult).Clone)
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
		"{exeState=%s, pid=%s, executionID=%s, exitCode=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s, healthProbeResults=%s, healthProbesEnabled=%s, stopAttemptInitiated=%t, startupStage=%s, startResults=%s}",
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
		ri.startupStage.String(),
		ri.startupResultsFriendlyString(),
	)
}

func (ri *ExecutableRunInfo) healthProbesFriendlyString() string {
	return logger.FriendlyStringSlice(maps.MapToSlice[string](ri.healthProbeResults, func(v apiv1.HealthProbeResult) string { return v.String() }))
}

func (ri *ExecutableRunInfo) startupResultsFriendlyString() string {
	return logger.FriendlyStringSlice(slices.Map[string](ri.startResults, (*ExecutableStartResult).String))
}

var _ fmt.Stringer = (*ExecutableRunInfo)(nil)
var _ Cloner[*ExecutableRunInfo] = (*ExecutableRunInfo)(nil)
var _ UpdateableFrom[*ExecutableRunInfo] = (*ExecutableRunInfo)(nil)
