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
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

// Stores information about Executable run
type runInfo struct {
	// State of the run (starting, running, finished, etc.)
	exeState apiv1.ExecutableState

	// Process ID of the process that runs the Executable
	pid *int64

	// Execution ID for the Executable (see ExecutableStatus for details)
	executionID string

	// Exit code of the Executable process
	exitCode *int32

	// Timestamp when the run was started
	startupTimestamp metav1.Time

	// Timestamp when the run was finished
	finishTimestamp metav1.Time

	// Paths to captured standard output and standard error files
	stdOutFile string
	stdErrFile string

	// The map of ports reserved for services that the Executable implements
	reservedPorts map[types.NamespacedName]int32
}

func NewRunInfo() *runInfo {
	return &runInfo{
		exeState:         "",
		pid:              apiv1.UnknownPID,
		exitCode:         apiv1.UnknownExitCode,
		startupTimestamp: metav1.Time{},
		finishTimestamp:  metav1.Time{},
	}
}

func (ri *runInfo) UpdateFrom(status apiv1.ExecutableStatus) {
	ri.exeState = status.State
	if status.PID != apiv1.UnknownPID {
		ri.pid = new(int64)
		*ri.pid = *status.PID
	}
	if status.ExecutionID != "" {
		ri.executionID = status.ExecutionID
	}
	if status.ExitCode != apiv1.UnknownExitCode {
		ri.exitCode = new(int32)
		*ri.exitCode = *status.ExitCode
	}
	if !status.StartupTimestamp.IsZero() {
		ri.startupTimestamp = status.StartupTimestamp
	}
	if !status.FinishTimestamp.IsZero() {
		ri.finishTimestamp = status.FinishTimestamp
	}
	if status.StdOutFile != "" {
		ri.stdOutFile = status.StdOutFile
	}
	if status.StdErrFile != "" {
		ri.stdErrFile = status.StdErrFile
	}
}

func (ri *runInfo) DeepCopy() *runInfo {
	retval := runInfo{
		exeState: ri.exeState,
	}
	if ri.pid != apiv1.UnknownPID {
		retval.pid = new(int64)
		*retval.pid = *ri.pid
	}
	retval.executionID = ri.executionID
	if ri.exitCode != apiv1.UnknownExitCode {
		retval.exitCode = new(int32)
		*retval.exitCode = *ri.exitCode
	}
	if len(ri.reservedPorts) > 0 {
		retval.reservedPorts = stdlib_maps.Clone(ri.reservedPorts)
	}
	retval.startupTimestamp = ri.startupTimestamp
	retval.finishTimestamp = ri.finishTimestamp
	retval.stdOutFile = ri.stdOutFile
	retval.stdErrFile = ri.stdErrFile
	return &retval
}

func (ri *runInfo) ApplyTo(exe *apiv1.Executable, log logr.Logger) objectChange {
	status := exe.Status
	originalStatusRI := NewRunInfo()
	originalStatusRI.UpdateFrom(status)
	changed := noChange

	if ri.exeState != "" && status.State != ri.exeState {
		status.State = ri.exeState
		changed = statusChanged
	}

	if ri.pid != apiv1.UnknownPID {
		if status.PID == apiv1.UnknownPID {
			status.PID = new(int64)
		}
		if *status.PID != *ri.pid {
			*status.PID = *ri.pid
			changed = statusChanged
		}
	}

	if ri.executionID != "" && status.ExecutionID != ri.executionID {
		status.ExecutionID = ri.executionID
		changed = statusChanged
	}

	if ri.exitCode != apiv1.UnknownExitCode {
		if status.ExitCode == apiv1.UnknownExitCode {
			status.ExitCode = new(int32)
		}
		if *status.ExitCode != *ri.exitCode {
			*status.ExitCode = *ri.exitCode
			changed = statusChanged
		}
	}

	// We only overwrite timestamps if the Executable status has them as zero values
	// to avoid round-tripping errors.
	if !ri.startupTimestamp.IsZero() && status.StartupTimestamp.IsZero() {
		status.StartupTimestamp = ri.startupTimestamp
		changed = statusChanged
	}

	if !ri.finishTimestamp.IsZero() && status.FinishTimestamp.IsZero() {
		status.FinishTimestamp = ri.finishTimestamp
		changed = statusChanged
	}

	if ri.stdOutFile != "" && status.StdOutFile != ri.stdOutFile {
		status.StdOutFile = ri.stdOutFile
		changed = statusChanged
	}

	if ri.stdErrFile != "" && status.StdErrFile != ri.stdErrFile {
		status.StdErrFile = ri.stdErrFile
		changed = statusChanged
	}

	if changed != noChange {
		exe.Status = status

		if log.V(1).Enabled() {
			log.V(1).Info("Executable run changed", "PropertiesChanged", DiffString(originalStatusRI, ri))
		}
	}

	return changed
}

func (ri *runInfo) String() string {
	return fmt.Sprintf(
		"{exeState=%s, pid=%s, executionID=%s, exitCode=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s}",
		ri.exeState,
		logger.IntPtrValToString(ri.pid),
		logger.FriendlyString(ri.executionID),
		logger.IntPtrValToString(ri.exitCode),
		logger.FriendlyMetav1Timestamp(ri.startupTimestamp),
		logger.FriendlyMetav1Timestamp(ri.finishTimestamp),
		logger.FriendlyString(ri.stdOutFile),
		logger.FriendlyString(ri.stdErrFile),
	)
}

func DiffString(r1, r2 *runInfo) string {
	sb := strings.Builder{}
	sb.WriteString("{")

	if r1.exeState != r2.exeState {
		sb.WriteString(fmt.Sprintf("exeState=%s->%s, ", r1.exeState, r2.exeState))
	}

	if !pointers.EqualValue(r1.pid, r2.pid) {
		sb.WriteString(fmt.Sprintf("pid=%s->%s, ", logger.IntPtrValToString(r1.pid), logger.IntPtrValToString(r2.pid)))
	}

	if r1.executionID != r2.executionID {
		sb.WriteString(fmt.Sprintf("executionID=%s->%s, ", logger.FriendlyString(r1.executionID), logger.FriendlyString(r2.executionID)))
	}

	if pointers.EqualValue(r1.exitCode, r2.exitCode) {
		sb.WriteString(fmt.Sprintf("exitCode=%s->%s, ", logger.IntPtrValToString(r1.exitCode), logger.IntPtrValToString(r2.exitCode)))
	}

	if r1.startupTimestamp != r2.startupTimestamp {
		sb.WriteString(fmt.Sprintf("startupTimestamp=%s->%s, ", logger.FriendlyMetav1Timestamp(r1.startupTimestamp), logger.FriendlyMetav1Timestamp(r2.startupTimestamp)))
	}

	if r1.finishTimestamp != r2.finishTimestamp {
		sb.WriteString(fmt.Sprintf("finishTimestamp=%s->%s, ", logger.FriendlyMetav1Timestamp(r1.finishTimestamp), logger.FriendlyMetav1Timestamp(r2.finishTimestamp)))
	}

	if r1.stdOutFile != r2.stdOutFile {
		sb.WriteString(fmt.Sprintf("stdOutFile=%s->%s, ", logger.FriendlyString(r1.stdOutFile), logger.FriendlyString(r2.stdOutFile)))
	}

	if r1.stdErrFile != r2.stdErrFile {
		sb.WriteString(fmt.Sprintf("stdErrFile=%s->%s, ", logger.FriendlyString(r1.stdErrFile), logger.FriendlyString(r2.stdErrFile)))
	}

	sb.WriteString("}")
	return sb.String()
}

var _ fmt.Stringer = (*runInfo)(nil)
