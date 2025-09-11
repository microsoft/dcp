// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"errors"
	"fmt"
	stdmaps "maps"
	"os"
	stdslices "slices"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/health"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
)

const (
	RuntimeContainerHealthProbeName = "__runtime"
)

// A structure representing startup log file with an associated ParagraphWriter.
// Having a shared writer enables separation of logs from multiple startup activities
// (image build, container creation, container start, network configuration).
type startupLog struct {
	file     *os.File
	writer   usvc_io.ParagraphWriter
	disposed atomic.Bool
}

func newStartupLog(ctr *apiv1.Container, logSource apiv1.LogStreamSource) (*startupLog, error) {
	var fileNameTemplate string
	switch logSource {
	case apiv1.LogStreamSourceStartupStdout:
		fileNameTemplate = "%s_startout_%s"
	case apiv1.LogStreamSourceStartupStderr:
		fileNameTemplate = "%s_starterr_%s"
	default:
		return nil, fmt.Errorf("unknown log source %v", logSource) // Should never happen
	}

	file, err := usvc_io.OpenTempFile(fmt.Sprintf(fileNameTemplate, ctr.Name, ctr.UID), os.O_RDWR|os.O_CREATE|os.O_APPEND, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		return nil, err
	}

	// Always append timestamp to startup logs; we'll strip them out if the streaming request doesn't ask for them
	writer := usvc_io.NewParagraphWriter(usvc_io.NewTimestampWriter(file), osutil.LineSep())

	return &startupLog{file: file, writer: writer}, nil
}

func (sl *startupLog) Close() error {
	return sl.writer.Close() // Will close the underlying file
}

func (sl *startupLog) Dispose() error {
	alive := sl.disposed.CompareAndSwap(false, true)
	if !alive {
		return nil
	}

	var disposeErr error
	closeErr := sl.Close()
	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		disposeErr = closeErr
	}

	if err := os.Remove(sl.file.Name()); err != nil {
		disposeErr = errors.Join(disposeErr, err)
	}
	return disposeErr
}

type containerNetworkConnectionKey struct {
	Container types.NamespacedName
	Network   types.NamespacedName
}

type containerID string

// Data that we keep, in memory, about running containers.
type runningContainerData struct {
	// The most recent state that we set on the Container object.
	// Note that the Container.Status.State may be lagging a bit because of caching in the K8s client libraries.
	containerState apiv1.ContainerState

	// This is the container startup error if container start fails.
	startupError error

	// If the container starts successfully, this is the container ID from the container orchestrator.
	containerID containerID

	// If the container starts successfully, this is the container name obtained from the container orchestrator.
	// Note that the orchestrator typically only returns container ID upon startup.
	// To obtain the container name, a container must be inspected.
	containerName string

	// True if the container stop attempt has been initiated/queued.
	stopAttemptInitiated bool

	// The exit code (if available) from the container.
	exitCode *int32

	// The most recent time the container finished running (either exited or failed to start).
	finishTimestamp metav1.MicroTime

	// Tracks whether startup has been attempted for the container
	startupAttempted bool

	// The time the start attempt finished (successfully or not).
	startAttemptFinishedAt metav1.MicroTime

	// Standard output content from startup commands will be saved to this log and read back when DCP clients request them.
	// Typically the file is filled during Container startup and then cleaned up when DCP is shut down,
	// or when the Container is deleted. There is a small chance
	// that the file might not be deleted if Container startup is in progress when DCP is asked to shut down,
	// but we do not consider this a problem worth slowing down the shutdown process.
	startupStdoutLog *startupLog

	// Standard error content from startup commands will be saved to this log.
	startupStderrLog *startupLog

	networkConnections map[containerNetworkConnectionKey]bool

	// The "run spec" that was used to start the container.
	// This is initialized with a copy of the Container.Spec, but then updated (the Env and Args part, specifically)
	// to include all value substitutions for environment variables and startup command arguments.
	runSpec *apiv1.ContainerSpec

	// The most recent health probe results (indexed by probe name)
	healthProbeResults map[string]apiv1.HealthProbeResult

	// Whether health probes are enabled for the Container
	healthProbesEnabled *bool
}

const placeholderContainerIdPrefix = "__placeholder-"

func newRunningContainerData(ctr *apiv1.Container) *runningContainerData {
	// For the sake of storing the runningContainerData in the runningContainers map we need
	// to have a unique container ID, so we generate a fake one here.
	// This ID won't be used for any real work.
	var placeholderID containerID
	if ctr.UID != "" {
		placeholderID = containerID(placeholderContainerIdPrefix + string(ctr.UID))
	} else {
		placeholderID = containerID(placeholderContainerIdPrefix + ctr.NamespacedName().String())
	}

	// Update the image name for container build scenarios
	runSpec := ctr.Spec.DeepCopy()
	runSpec.Image = ctr.SpecifiedImageNameOrDefault()

	return &runningContainerData{
		containerState:     apiv1.ContainerStatePending,
		containerID:        placeholderID,
		networkConnections: make(map[containerNetworkConnectionKey]bool),
		runSpec:            runSpec,
		exitCode:           apiv1.UnknownExitCode,
		healthProbeResults: make(map[string]apiv1.HealthProbeResult),
	}
}

// Returns another instance of runningContainerData with the same data.
// The copy is safe to update independently of the original EXCEPT for the startup log pointers,
// which are shared between the original and the copy.
func (rcd *runningContainerData) Clone() *runningContainerData {
	clone := runningContainerData{
		containerState:         rcd.containerState,
		startupError:           rcd.startupError,
		containerID:            rcd.containerID,
		containerName:          rcd.containerName,
		stopAttemptInitiated:   rcd.stopAttemptInitiated,
		finishTimestamp:        rcd.finishTimestamp,
		startupAttempted:       rcd.startupAttempted,
		startAttemptFinishedAt: rcd.startAttemptFinishedAt,
		startupStdoutLog:       rcd.startupStdoutLog,
		startupStderrLog:       rcd.startupStderrLog,
		networkConnections:     stdmaps.Clone(rcd.networkConnections),
		runSpec:                rcd.runSpec.DeepCopy(),
		healthProbeResults:     stdmaps.Clone(rcd.healthProbeResults),
	}

	pointers.SetValueFrom(&clone.exitCode, rcd.exitCode)
	pointers.SetValueFrom(&clone.healthProbesEnabled, rcd.healthProbesEnabled)

	return &clone
}

func (rcd *runningContainerData) UpdateFrom(other *runningContainerData) bool {
	if other == nil {
		return false
	}

	updated := false

	if rcd.containerState != other.containerState {
		rcd.containerState = other.containerState
		updated = true
	}

	if rcd.startupError != other.startupError {
		// Note: this is strict equality comparison (same error type and instance).
		// Two different instances of the same type of error will be considered "different",
		// as will be "untyped nil error" vs "typed nil error".
		rcd.startupError = other.startupError
		updated = true
	}

	if rcd.containerID != other.containerID {
		rcd.containerID = other.containerID
		updated = true
	}

	if rcd.containerName != other.containerName {
		rcd.containerName = other.containerName
		updated = true
	}

	if rcd.stopAttemptInitiated != other.stopAttemptInitiated {
		rcd.stopAttemptInitiated = other.stopAttemptInitiated
		updated = true
	}

	if !pointers.EqualValue(rcd.exitCode, other.exitCode) {
		pointers.SetValueFrom(&rcd.exitCode, other.exitCode)
		updated = true
	}

	if !rcd.finishTimestamp.Equal(&other.finishTimestamp) {
		rcd.finishTimestamp = other.finishTimestamp
		updated = true
	}

	if rcd.startupAttempted != other.startupAttempted {
		rcd.startupAttempted = other.startupAttempted
		updated = true
	}

	if !rcd.startAttemptFinishedAt.Equal(&other.startAttemptFinishedAt) {
		rcd.startAttemptFinishedAt = other.startAttemptFinishedAt
		updated = true
	}

	if rcd.startupStdoutLog != other.startupStdoutLog {
		rcd.startupStdoutLog = other.startupStdoutLog
		updated = true
	}

	if rcd.startupStderrLog != other.startupStderrLog {
		rcd.startupStderrLog = other.startupStderrLog
		updated = true
	}

	if !stdmaps.Equal(rcd.networkConnections, other.networkConnections) {
		rcd.networkConnections = stdmaps.Clone(other.networkConnections)
		updated = true
	}

	if !rcd.runSpec.Equal(other.runSpec) {
		rcd.runSpec = other.runSpec.DeepCopy()
		updated = true
	}

	if len(other.healthProbeResults) > 0 {
		updated = maps.Insert(rcd.healthProbeResults, stdmaps.All(other.healthProbeResults)) || updated
	}

	if other.healthProbesEnabled != nil && !pointers.EqualValue(rcd.healthProbesEnabled, other.healthProbesEnabled) {
		pointers.SetValueFrom(&rcd.healthProbesEnabled, other.healthProbesEnabled)
		updated = true
	}

	return updated
}

func (rcd *runningContainerData) ensureStartupLogFiles(ctr *apiv1.Container, log logr.Logger) {
	if rcd.startupStdoutLog != nil || rcd.startupStderrLog != nil {
		log.V(1).Info("Startup output files already created")
		return
	}

	stdoutLog, stdoutLogErr := newStartupLog(ctr, apiv1.LogStreamSourceStartupStdout)
	if stdoutLogErr != nil {
		log.Error(stdoutLogErr, "Failed to create temporary file for capturing container startup standard output data", "Container", ctr.NamespacedName().String())
	} else {
		rcd.startupStdoutLog = stdoutLog
	}

	stderrLog, stderrLogErr := newStartupLog(ctr, apiv1.LogStreamSourceStartupStderr)
	if stderrLogErr != nil {
		log.Error(stderrLogErr, "Failed to create temporary file for capturing container startup standard error data", "Container", ctr.NamespacedName().String())
	} else {
		rcd.startupStderrLog = stderrLog
	}
}

func (rcd *runningContainerData) closeStartupLogFiles(log logr.Logger) {
	stdoutLog := rcd.startupStdoutLog
	if stdoutLog != nil {
		closeErr := stdoutLog.Close()
		if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "Failed to close startup standard output file")
		}
	}

	stderrLog := rcd.startupStderrLog
	if stderrLog != nil {
		closeErr := stderrLog.Close()
		if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "Failed to close startup standard error file")
		}
	}
}

func (rcd *runningContainerData) deleteStartupLogFiles(_ logr.Logger) {
	stdoutLog := rcd.startupStdoutLog
	rcd.startupStdoutLog = nil
	if stdoutLog != nil {
		// Best effort. In particular, if multiple clones of runningContainerData share a file,
		// only first deletion attempt will succeed.
		_ = stdoutLog.Dispose()
	}

	stderrLog := rcd.startupStderrLog
	rcd.startupStderrLog = nil
	if stderrLog != nil {
		// Best effort, see comment above.
		_ = stderrLog.Dispose()
	}
}

func (rcd *runningContainerData) getStartupLogWriters() (usvc_io.ParagraphWriter, usvc_io.ParagraphWriter) {
	var stdoutWriter, stderrWriter usvc_io.ParagraphWriter

	stdoutLog := rcd.startupStdoutLog
	if stdoutLog != nil {
		stdoutWriter = stdoutLog.writer
	}

	stderrLog := rcd.startupStderrLog
	if stderrLog != nil {
		stderrWriter = stderrLog.writer
	}

	return stdoutWriter, stderrWriter
}

func (rcd *runningContainerData) hasValidContainerID() bool {
	return rcd.containerID != "" && !strings.HasPrefix(string(rcd.containerID), placeholderContainerIdPrefix)
}

func (rcd *runningContainerData) updateFromInspectedContainer(inspected *ct.InspectedContainer) {
	rcd.containerID = containerID(inspected.Id)
	rcd.containerName = strings.TrimLeft(inspected.Name, "/")
	rcd.runSpec.Env = maps.MapToSlice[apiv1.EnvVar](inspected.Env, func(key, value string) apiv1.EnvVar {
		return apiv1.EnvVar{Name: key, Value: value}
	})
	rcd.runSpec.Args = inspected.Args

	switch inspected.Status {
	case ct.ContainerStatusCreated:
		rcd.containerState = apiv1.ContainerStateStarting
	case ct.ContainerStatusRunning, ct.ContainerStatusRestarting:
		rcd.containerState = apiv1.ContainerStateRunning
	case ct.ContainerStatusPaused:
		rcd.containerState = apiv1.ContainerStatePaused
	case ct.ContainerStatusRemoving:
		rcd.containerState = apiv1.ContainerStateStopping
	case ct.ContainerStatusExited, ct.ContainerStatusDead:
		rcd.containerState = apiv1.ContainerStateExited
	}

	if len(inspected.Healthcheck) > 0 {
		// If the container has a healthcheck command configured, we need to report a result for it.
		// We do this even if the healthcheck hasn't run yet to allow the container to be reported
		// as caution instead of healthy or unhealthy until the actual health check result is available.
		result := apiv1.HealthProbeResult{
			ProbeName: RuntimeContainerHealthProbeName,
			Outcome:   apiv1.HealthProbeOutcomeUnknown,
		}

		if inspected.Health != nil {
			// If the container has health information available, update
			// the health probe result to include it.
			switch inspected.Health.Status {
			case "healthy":
				result.Outcome = apiv1.HealthProbeOutcomeSuccess
			case "unhealthy":
				result.Outcome = apiv1.HealthProbeOutcomeFailure
			}

			for _, log := range inspected.Health.Log {
				if log.End.After(result.Timestamp.Time) {
					result.Timestamp = metav1.NewMicroTime(log.End)
					result.Reason = log.Output
				}
			}
		}

		if result.Timestamp.IsZero() {
			result.Timestamp = rcd.startAttemptFinishedAt
		}

		rcd.healthProbeResults[RuntimeContainerHealthProbeName] = result
	}

	rcd.finishTimestamp = metav1.NewMicroTime(inspected.FinishedAt)
	if !rcd.finishTimestamp.IsZero() {
		pointers.SetValue(&rcd.exitCode, inspected.ExitCode)
	}
}

func (rcd *runningContainerData) applyTo(ctr *apiv1.Container, log logr.Logger) objectChange {
	change := noChange

	if rcd.containerState != apiv1.ContainerStateEmpty && rcd.containerState != ctr.Status.State {
		ctr.Status.State = rcd.containerState
		change |= statusChanged
	}

	if rcd.hasValidContainerID() && ctr.Status.ContainerID != string(rcd.containerID) {
		ctr.Status.ContainerID = string(rcd.containerID)
		change |= statusChanged
	}

	if rcd.containerName != "" && ctr.Status.ContainerName != rcd.containerName {
		ctr.Status.ContainerName = rcd.containerName
		change |= statusChanged
	}

	if !pointers.EqualValue(rcd.exitCode, ctr.Status.ExitCode) {
		pointers.SetValueFrom(&ctr.Status.ExitCode, rcd.exitCode)
		change |= statusChanged
	}

	if rcd.containerState == apiv1.ContainerStateFailedToStart && rcd.startupError != nil {
		errMsg := fmt.Sprintf("Container startup failed: %v", rcd.startupError)
		if ctr.Status.Message != errMsg {
			ctr.Status.Message = errMsg
			change |= statusChanged
		}
	}

	startupStoutLog := rcd.startupStdoutLog
	if startupStoutLog != nil && ctr.Status.StartupStdOutFile == "" {
		ctr.Status.StartupStdOutFile = startupStoutLog.file.Name()
		change |= statusChanged
	}

	startupStderrLog := rcd.startupStderrLog
	if startupStderrLog != nil && ctr.Status.StartupStdErrFile == "" {
		ctr.Status.StartupStdErrFile = startupStderrLog.file.Name()
		change |= statusChanged
	}

	// From the runSpec the runningContainerData has, only Env and Args will ever be modified
	// (as a result of evaluating template expressions that may be embedded in environment variables or command arguments).

	// For comparison between the environment maps, we need to convert them to map[string]string.
	rawContainerEnv := maps.SliceToMap(ctr.Status.EffectiveEnv, func(ev apiv1.EnvVar) (string, string) {
		return ev.Name, ev.Value
	})
	rawCurrentEnv := maps.SliceToMap(rcd.runSpec.Env, func(ev apiv1.EnvVar) (string, string) {
		return ev.Name, ev.Value
	})
	if len(rawCurrentEnv) > 0 && !stdmaps.Equal(rawContainerEnv, rawCurrentEnv) {
		rawContainerEnv = maps.Apply(rawContainerEnv, rawCurrentEnv)
		ctr.Status.EffectiveEnv = maps.MapToSlice[apiv1.EnvVar](rawContainerEnv, func(k, v string) apiv1.EnvVar {
			return apiv1.EnvVar{Name: k, Value: v}
		})
		change |= statusChanged
	}

	if len(rcd.runSpec.Args) > 0 && !stdslices.Equal(ctr.Status.EffectiveArgs, rcd.runSpec.Args) {
		ctr.Status.EffectiveArgs = rcd.runSpec.Args
		change |= statusChanged
	}

	if ctr.Status.StartupTimestamp.IsZero() && !rcd.startAttemptFinishedAt.IsZero() {
		ctr.Status.StartupTimestamp = rcd.startAttemptFinishedAt
		change |= statusChanged
	}

	if ctr.Status.LifecycleKey == "" {
		lifecycleKey, _, hashErr := rcd.runSpec.GetLifecycleKey()
		if hashErr != nil {
			change |= additionalReconciliationNeeded
		} else {
			ctr.Status.LifecycleKey = lifecycleKey
			change |= statusChanged
		}
	}

	if setTimestampIfBeforeOrUnknown(rcd.finishTimestamp, &ctr.Status.FinishTimestamp) {
		change |= statusChanged
	} else if rcd.containerState == apiv1.ContainerStateFailedToStart && setTimestampIfBeforeOrUnknown(rcd.startAttemptFinishedAt, &ctr.Status.FinishTimestamp) {
		change |= statusChanged
	}

	updatedHealthResults, healthResultsChanged := health.UpdateHealthProbeResults(ctr.Status.HealthProbeResults, rcd.healthProbeResults)
	if healthResultsChanged {
		ctr.Status.HealthProbeResults = updatedHealthResults
		change |= statusChanged
	}

	change |= updateContainerHealthStatus(ctr, rcd.containerState, log)

	return change
}

var _ Cloner[*runningContainerData] = (*runningContainerData)(nil)
var _ UpdateableFrom[*runningContainerData] = (*runningContainerData)(nil)
