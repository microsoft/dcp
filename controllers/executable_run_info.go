// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"fmt"
	stdlib_maps "maps"
	"slices"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/usvc-apiserver/internal/health"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	usvc_slices "github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Stores information about Executable run
type ExecutableRunInfo struct {
	// Namespaced name of the Executable
	namespacedName types.NamespacedName

	*apiv1.ExecutableStatus

	// The map of ports reserved for services that the Executable implements
	reservedPorts map[types.NamespacedName]int32

	// Whether health probes are enabled for the Executable
	healthProbesEnabled bool

	mutex *sync.Mutex
}

func NewRunInfo(exe *apiv1.Executable) *ExecutableRunInfo {
	return &ExecutableRunInfo{
		namespacedName:   exe.NamespacedName(),
		ExecutableStatus: exe.Status.DeepCopy(),
		mutex:            &sync.Mutex{},
	}
}

func (ri *ExecutableRunInfo) NamespacedName() types.NamespacedName {
	return ri.namespacedName
}

func (ri *ExecutableRunInfo) DeepCopy() *ExecutableRunInfo {
	retval := &ExecutableRunInfo{
		namespacedName:      ri.namespacedName,
		ExecutableStatus:    ri.ExecutableStatus.DeepCopy(),
		healthProbesEnabled: ri.healthProbesEnabled,
		mutex:               &sync.Mutex{},
	}
	if len(ri.reservedPorts) > 0 {
		retval.reservedPorts = stdlib_maps.Clone(ri.reservedPorts)
	}
	return retval
}

func (ri *ExecutableRunInfo) ApplyTo(exe *apiv1.Executable, log logr.Logger) objectChange {
	changed := noChange

	status := &exe.Status

	if status.State != ri.State {
		status.State = ri.State
		changed = statusChanged
	}

	if ri.PID != apiv1.UnknownPID && (status.PID == apiv1.UnknownPID || *status.PID != *ri.PID) {
		status.PID = new(int64)
		*status.PID = *ri.PID
		changed = statusChanged
	}

	if ri.ExecutionID != "" && status.ExecutionID != ri.ExecutionID {
		status.ExecutionID = ri.ExecutionID
		changed = statusChanged
	}

	if ri.ExitCode != apiv1.UnknownExitCode && (status.ExitCode == apiv1.UnknownExitCode || *status.ExitCode != *ri.ExitCode) {
		status.ExitCode = new(int32)
		*status.ExitCode = *ri.ExitCode
		changed = statusChanged
	}

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

	if len(ri.EffectiveEnv) != 0 && (len(status.EffectiveEnv) == 0 || !slices.Equal(ri.EffectiveEnv, status.EffectiveEnv)) {
		status.EffectiveEnv = make([]apiv1.EnvVar, len(ri.EffectiveEnv))
		copy(status.EffectiveEnv, ri.EffectiveEnv)
		changed = statusChanged
	} else if len(ri.EffectiveEnv) == 0 && len(status.EffectiveEnv) != 0 {
		status.EffectiveEnv = make([]apiv1.EnvVar, 0)
		changed = statusChanged
	}

	if len(ri.EffectiveArgs) != 0 && (len(status.EffectiveArgs) == 0 || !slices.Equal(ri.EffectiveArgs, status.EffectiveArgs)) {
		status.EffectiveArgs = make([]string, len(ri.EffectiveArgs))
		copy(status.EffectiveArgs, ri.EffectiveArgs)
		changed = statusChanged
	} else if len(ri.EffectiveArgs) == 0 && len(status.EffectiveArgs) != 0 {
		status.EffectiveArgs = make([]string, 0)
		changed = statusChanged
	}

	healthResultsChanged := false
	statusResults := maps.SliceToMap(status.HealthProbeResults, func(hpr apiv1.HealthProbeResult) (string, apiv1.HealthProbeResult) {
		return hpr.ProbeName, hpr
	})
	if statusResults == nil {
		statusResults = make(map[string]apiv1.HealthProbeResult)
	}
	for _, hpr := range ri.HealthProbeResults {
		statusHpr, found := statusResults[hpr.ProbeName]
		if !found {
			statusResults[hpr.ProbeName] = hpr
			healthResultsChanged = true
		} else {
			res, timestampDiff := hpr.Diff(&statusHpr)
			timestampDiff = timestampDiff.Abs()
			if res == apiv1.Different || (res == apiv1.DiffTimestampOnly && timestampDiff >= maxStaleHealthProbeResultAge) {
				statusResults[hpr.ProbeName] = hpr
				healthResultsChanged = true
			}
		}
	}
	if healthResultsChanged {
		status.HealthProbeResults = maps.Values(statusResults)
		changed = statusChanged
	}

	oldHealthStatus := status.HealthStatus
	newHealthStatus := apiv1.HealthStatusUnhealthy
	switch ri.State {
	case apiv1.ExecutableStateEmpty, apiv1.ExecutableStateStarting, apiv1.ExecutableStateTerminated, apiv1.ExecutableStateUnknown:
		newHealthStatus = apiv1.HealthStatusCaution

	case apiv1.ExecutableStateRunning:
		if len(exe.Spec.HealthProbes) == 0 {
			newHealthStatus = apiv1.HealthStatusHealthy
		} else {
			newHealthStatus = health.HealthStatusFromProbeResults(exe.Status.HealthProbeResults)
		}

	case apiv1.ExecutableStateFailedToStart:
		newHealthStatus = apiv1.HealthStatusUnhealthy

	case apiv1.ExecutableStateFinished:
		if exe.Status.ExitCode == apiv1.UnknownExitCode || *exe.Status.ExitCode == 0 {
			newHealthStatus = apiv1.HealthStatusCaution
		} else {
			newHealthStatus = apiv1.HealthStatusUnhealthy
		}
	}

	if oldHealthStatus != newHealthStatus {
		status.HealthStatus = newHealthStatus
		log.V(1).Info("Health status changed", "OldHealthStatus", oldHealthStatus, "NewHealthStatus", status.HealthStatus)
	}

	return changed
}

func (ri *ExecutableRunInfo) Lock() {
	ri.mutex.Lock()
}

func (ri *ExecutableRunInfo) Unlock() {
	ri.mutex.Unlock()
}

func (ri *ExecutableRunInfo) SetState(state apiv1.ExecutableState) bool {
	if ri.State.CanUpdateTo(state) {
		ri.State = state
		if state.IsTerminal() && ri.FinishTimestamp.IsZero() {
			ri.FinishTimestamp = metav1.NowMicro()
		}

		return true
	}

	return false
}

func (ri *ExecutableRunInfo) String() string {
	return fmt.Sprintf(
		"{state=%s, pid=%s, executionID=%s, exitCode=%s, startupTimestamp=%s, finishTimestamp=%s, stdOutFile=%s, stdErrFile=%s, healthProbeResults=%s, healthProbesEnabled=%t}",
		ri.State,
		logger.IntPtrValToString(ri.PID),
		logger.FriendlyString(ri.ExecutionID),
		logger.IntPtrValToString(ri.ExitCode),
		logger.FriendlyMetav1Timestamp(ri.StartupTimestamp),
		logger.FriendlyMetav1Timestamp(ri.FinishTimestamp),
		logger.FriendlyString(ri.StdOutFile),
		logger.FriendlyString(ri.StdErrFile),
		ri.healthProbesFriendlyString(),
		ri.healthProbesEnabled,
	)
}

func DiffString(r1, r2 *ExecutableRunInfo) string {
	sb := strings.Builder{}

	sb.WriteString("{")

	if r1.State != r2.State {
		sb.WriteString(fmt.Sprintf("state=%s->%s, ", r1.State, r2.State))
	}

	if !pointers.EqualValue(r1.PID, r2.PID) {
		sb.WriteString(fmt.Sprintf("pid=%s->%s, ", logger.IntPtrValToString(r1.PID), logger.IntPtrValToString(r2.PID)))
	}

	if r1.ExecutionID != r2.ExecutionID {
		sb.WriteString(fmt.Sprintf("executionID=%s->%s, ", logger.FriendlyString(r1.ExecutionID), logger.FriendlyString(r2.ExecutionID)))
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

	if !slices.Equal(r1.HealthProbeResults, r2.HealthProbeResults) {
		sb.WriteString(fmt.Sprintf("healthProbeResults=%s->%s, ", r1.healthProbesFriendlyString(), r2.healthProbesFriendlyString()))
	}

	if r1.healthProbesEnabled != r2.healthProbesEnabled {
		sb.WriteString(fmt.Sprintf("healthProbesEnabled=%t->%t, ", r1.healthProbesEnabled, r2.healthProbesEnabled))
	}

	sb.WriteString("}")
	return sb.String()
}

func (ri *ExecutableRunInfo) healthProbesFriendlyString() string {
	return logger.FriendlyStringSlice(usvc_slices.Map[apiv1.HealthProbeResult, string](ri.HealthProbeResults, func(v apiv1.HealthProbeResult) string { return v.String() }))
}

var _ fmt.Stringer = (*ExecutableRunInfo)(nil)
