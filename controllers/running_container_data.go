// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"crypto/sha256"
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
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
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

// Data that we keep, in memory, about running containers.
type runningContainerData struct {
	// The most recent state that we set on the Container object.
	// Note that the Container.Status.State may be lagging a bit because of caching in the K8s client libraries.
	containerState apiv1.ContainerState

	// This is the container startup error if container start fails.
	startupError error

	// If the container starts successfully, this is the container ID from the container orchestrator.
	containerID string

	// If the container starts successfully, this is the container name obtained from the container orchestrator.
	// Note that the orchestrator typically only returns container ID upon startup.
	// To obtain the container name, a container must be inspected.
	containerName string

	// True if the contaienr stop attempt has been initiated/queued.
	stopAttemptInitiated bool

	// The exit code (if available) from the container.
	exitCode *int32

	// Tracks whether startup has been attempted for the container
	startupAttempted bool

	// Standard output content from startup commands will be saved to this log and read back when DCP clients request them.
	// Atomic pointer is used because the underlying file/writer combo is manipulated by the controller
	// and the shutdown handler, which may act on it concurrently. Same applies to the startup stderr log below.
	//
	// Typically the file is filled during Container startup and then cleaned up when DCP is shut down,
	// or when the Container is deleted. There is a small chance
	// that the file might not be deleted if Container startup is in progress when DCP is asked to shut down,
	// but we do not consider this a problem worth slowing down the shutdown process.
	startupStdoutLog atomic.Pointer[startupLog]

	// Standard error content from startup commands will be saved to this log.
	startupStderrLog atomic.Pointer[startupLog]

	// The time the start attempt finished (successfully or not).
	startAttemptFinishedAt metav1.MicroTime

	// The map of ports reserved for services that the Container implements
	reservedPorts map[types.NamespacedName]int32

	// The "run spec" that was used to start the container.
	// This is initialized with a copy of the Container.Spec, but then updated (the Env and Args part, specifically)
	// to include all value substitutions for environment variables and startup command arguments.
	runSpec *apiv1.ContainerSpec
}

const placeholderContainerIdPrefix = "__placeholder-"

func newRunningContainerData(ctr *apiv1.Container) *runningContainerData {
	// For the sake of storing the runningContainerData in the runningContainers map we need
	// to have a unique container ID, so we generate a fake one here.
	// This ID won't be used for any real work.
	var placeholderID string
	if ctr.UID != "" {
		placeholderID = placeholderContainerIdPrefix + string(ctr.UID)
	} else {
		placeholderID = placeholderContainerIdPrefix + ctr.NamespacedName().String()
	}

	// Update the image name for container build scenarios
	runSpec := ctr.Spec.DeepCopy()
	runSpec.Image = ctr.SpecifiedImageNameOrDefault()

	return &runningContainerData{
		containerState: apiv1.ContainerStatePending,
		containerID:    placeholderID,
		reservedPorts:  make(map[types.NamespacedName]int32),
		runSpec:        runSpec,
		exitCode:       apiv1.UnknownExitCode,
	}
}

// Returns another instance of runningContainerData with the same data.
// The copy is safe to update independently of the original EXCEPT for the startup log pointers,
// which are shared between the original and the copy.
func (rcd *runningContainerData) clone() *runningContainerData {
	clone := runningContainerData{
		containerState:         rcd.containerState,
		startupError:           rcd.startupError,
		containerID:            rcd.containerID,
		containerName:          rcd.containerName,
		stopAttemptInitiated:   rcd.stopAttemptInitiated,
		startAttemptFinishedAt: rcd.startAttemptFinishedAt,
		reservedPorts:          stdmaps.Clone(rcd.reservedPorts),
		runSpec:                rcd.runSpec.DeepCopy(),
		startupAttempted:       rcd.startupAttempted,
	}

	if rcd.exitCode != nil {
		clone.exitCode = new(int32)
		*clone.exitCode = *rcd.exitCode
	}
	clone.startupStderrLog.Store(rcd.startupStderrLog.Load())
	clone.startupStdoutLog.Store(rcd.startupStdoutLog.Load())

	return &clone
}

func (rcd *runningContainerData) ensureStartupLogFiles(ctr *apiv1.Container, log logr.Logger) {
	if rcd.startupStdoutLog.Load() != nil || rcd.startupStderrLog.Load() != nil {
		log.V(1).Info("startup output files already created")
		return
	}

	stdoutLog, stdoutLogErr := newStartupLog(ctr, apiv1.LogStreamSourceStartupStdout)
	if stdoutLogErr != nil {
		log.Error(stdoutLogErr, "failed to create temporary file for capturing container startup standard output data", "Container", ctr.NamespacedName().String())
	} else {
		swapped := rcd.startupStdoutLog.CompareAndSwap(nil, stdoutLog)
		if !swapped {
			// This can happen if shutdown coincides with creation of a Container.
			_ = stdoutLog.Dispose()
		}
	}

	stderrLog, stderrLogErr := newStartupLog(ctr, apiv1.LogStreamSourceStartupStderr)
	if stderrLogErr != nil {
		log.Error(stderrLogErr, "failed to create temporary file for capturing container startup standard error data", "Container", ctr.NamespacedName().String())
	} else {
		swapped := rcd.startupStderrLog.CompareAndSwap(nil, stderrLog)
		if !swapped {
			_ = stderrLog.Dispose()
		}
	}
}

func (rcd *runningContainerData) closeStartupLogFiles(log logr.Logger) {
	stdoutLog := rcd.startupStdoutLog.Load()
	if stdoutLog != nil {
		closeErr := stdoutLog.Close()
		if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "failed to close startup standard output file")
		}
	}

	stderrLog := rcd.startupStderrLog.Load()
	if stderrLog != nil {
		closeErr := stderrLog.Close()
		if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "failed to close startup standard error file")
		}
	}
}

func (rcd *runningContainerData) deleteStartupLogFiles(_ logr.Logger) {
	stdoutLog := rcd.startupStdoutLog.Swap(nil)
	if stdoutLog != nil {
		// Best effort. In particular, if multiple clones of runningContainerData share a file,
		// only first deletion attempt will succeed.
		_ = stdoutLog.Dispose()
	}

	stderrLog := rcd.startupStderrLog.Swap(nil)
	if stderrLog != nil {
		// Best effort, see comment above.
		_ = stderrLog.Dispose()
	}
}

func (rcd *runningContainerData) getStartupLogWriters() (usvc_io.ParagraphWriter, usvc_io.ParagraphWriter) {
	var stdoutWriter, stderrWriter usvc_io.ParagraphWriter

	stdoutLog := rcd.startupStdoutLog.Load()
	if stdoutLog != nil {
		stdoutWriter = stdoutLog.writer
	}

	stderrLog := rcd.startupStderrLog.Load()
	if stderrLog != nil {
		stderrWriter = stderrLog.writer
	}

	return stdoutWriter, stderrWriter
}

func (rcd *runningContainerData) hasValidContainerID() bool {
	return rcd.containerID != "" && !strings.HasPrefix(rcd.containerID, placeholderContainerIdPrefix)
}

func (rcd *runningContainerData) updateFromInspectedContainer(inspected *ct.InspectedContainer) {
	rcd.containerID = inspected.Id
	rcd.containerName = strings.TrimLeft(inspected.Name, "/")
	rcd.runSpec.Env = maps.MapToSlice[string, string, apiv1.EnvVar](inspected.Env, func(key, value string) apiv1.EnvVar {
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
		rcd.exitCode = new(int32)
		*rcd.exitCode = inspected.ExitCode
	}
}

func (rcd *runningContainerData) applyTo(ctr *apiv1.Container) objectChange {
	change := setContainerState(ctr, rcd.containerState)

	if rcd.hasValidContainerID() && ctr.Status.ContainerID != rcd.containerID {
		ctr.Status.ContainerID = rcd.containerID
		change |= statusChanged
	}

	if rcd.containerName != "" && ctr.Status.ContainerName != rcd.containerName {
		ctr.Status.ContainerName = rcd.containerName
		change |= statusChanged
	}

	if rcd.exitCode != apiv1.UnknownExitCode {
		if ctr.Status.ExitCode == apiv1.UnknownExitCode {
			ctr.Status.ExitCode = new(int32)
		}
		if *ctr.Status.ExitCode != *rcd.exitCode {
			*ctr.Status.ExitCode = *rcd.exitCode
			change |= statusChanged
		}
	}

	if rcd.containerState == apiv1.ContainerStateFailedToStart && rcd.startupError != nil {
		errMsg := fmt.Sprintf("Container startup failed: %v", rcd.startupError)
		if ctr.Status.Message != errMsg {
			ctr.Status.Message = errMsg
			change |= statusChanged
		}
	}

	startupStoutLog := rcd.startupStdoutLog.Load()
	if startupStoutLog != nil && ctr.Status.StartupStdOutFile == "" {
		ctr.Status.StartupStdOutFile = startupStoutLog.file.Name()
		change |= statusChanged
	}

	startupStderrLog := rcd.startupStderrLog.Load()
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
		ctr.Status.EffectiveEnv = maps.MapToSlice[string, string, apiv1.EnvVar](rawContainerEnv, func(k, v string) apiv1.EnvVar {
			return apiv1.EnvVar{Name: k, Value: v}
		})
		change |= statusChanged
	}

	if len(rcd.runSpec.Args) > 0 && !stdslices.Equal(ctr.Status.EffectiveArgs, rcd.runSpec.Args) {
		ctr.Status.EffectiveArgs = rcd.runSpec.Args
		change |= statusChanged
	}

	// We overwrite timestamps only if they are not already set, to avoid problems with rounding errors.

	if ctr.Status.StartupTimestamp.IsZero() && !rcd.startAttemptFinishedAt.IsZero() {
		ctr.Status.StartupTimestamp = rcd.startAttemptFinishedAt
		// Set the lifecycle key once the container has started
		ctr.Status.LifecycleKey = rcd.getLifecycleKey()
		change |= statusChanged
	}

	if ctr.Status.FinishTimestamp.IsZero() {
		if rcd.containerState == apiv1.ContainerStateExited {
			ctr.Status.FinishTimestamp = metav1.NowMicro()
			change |= statusChanged
		} else if rcd.containerState == apiv1.ContainerStateFailedToStart && !rcd.startAttemptFinishedAt.IsZero() {
			ctr.Status.FinishTimestamp = rcd.startAttemptFinishedAt
			change |= statusChanged
		}
	}

	return change
}

func (rcd *runningContainerData) getLifecycleKey() string {
	if rcd.runSpec.LifecycleKey != "" {
		return rcd.runSpec.LifecycleKey
	}

	// Concatinate fields that can necessitate recreating a container if changed
	configString := fmt.Sprintf(
		"%s%v%v%v%v%s%s%v",
		rcd.runSpec.Image, // Use the evaluated image name (which could be an image ID if a build context is specified)
		rcd.runSpec.VolumeMounts,
		rcd.runSpec.Ports,
		rcd.runSpec.Env, // This is the evaluated environment, not the raw environment
		rcd.runSpec.EnvFiles,
		rcd.runSpec.RestartPolicy,
		rcd.runSpec.Command,
		rcd.runSpec.Args, // This is the evaluated arguments, not the raw arguments
	)

	return fmt.Sprintf("%x", sha256.Sum256([]byte(configString)))
}
