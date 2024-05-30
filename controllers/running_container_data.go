// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"errors"
	"fmt"
	stdmaps "maps"
	"os"
	stdslices "slices"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	ct "github.com/microsoft/usvc-apiserver/internal/containers"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

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

	// Standard output from startup commands will be streamed to this file for log streaming support.
	startupStdOutFile *os.File

	// Standard error from startup commands will be streamed to this file for log streaming support.
	startupStdErrFile *os.File

	// The time the start attempt finished (successfully or not).
	startAttemptFinishedAt metav1.Time

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
// The copy is safe to update independently of the original EXCEPT for the startup file pointers,
// which are shared between the original and the copy.
func (rcd *runningContainerData) clone() *runningContainerData {
	clone := runningContainerData{
		containerState:         rcd.containerState,
		startupError:           rcd.startupError,
		containerID:            rcd.containerID,
		containerName:          rcd.containerName,
		stopAttemptInitiated:   rcd.stopAttemptInitiated,
		startupStdOutFile:      rcd.startupStdOutFile,
		startupStdErrFile:      rcd.startupStdErrFile,
		startAttemptFinishedAt: rcd.startAttemptFinishedAt,
		reservedPorts:          stdmaps.Clone(rcd.reservedPorts),
		runSpec:                rcd.runSpec.DeepCopy(),
		startupAttempted:       rcd.startupAttempted,
	}

	if rcd.exitCode != nil {
		clone.exitCode = new(int32)
		*clone.exitCode = *rcd.exitCode
	}

	return &clone
}

func (rcd *runningContainerData) ensureStartupOutputFiles(ctr *apiv1.Container, log logr.Logger) {
	if rcd.startupStdOutFile != nil || rcd.startupStdErrFile != nil {
		log.V(1).Info("startup output files already created")
		return
	}

	// We might perform multiple startup attempts, so if the file(s) already exist, we will reuse them.

	stdOutFile, err := usvc_io.OpenTempFile(fmt.Sprintf("%s_startout_%s", ctr.Name, ctr.UID), os.O_RDWR|os.O_CREATE|os.O_APPEND, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing container startup standard output data")
		rcd.startupStdOutFile = nil
	} else {
		rcd.startupStdOutFile = stdOutFile
	}

	stdErrFile, err := usvc_io.OpenTempFile(fmt.Sprintf("%s_starterr_%s", ctr.Name, ctr.UID), os.O_RDWR|os.O_CREATE|os.O_APPEND, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing container startup standard error data")
		rcd.startupStdErrFile = nil
	} else {
		rcd.startupStdErrFile = stdErrFile
	}
}

func (rcd *runningContainerData) closeStartupLogFiles(log logr.Logger) {
	if rcd.startupStdOutFile != nil {
		closeErr := rcd.startupStdOutFile.Close()
		if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "failed to close startup standard output file")
		}
	}

	if rcd.startupStdErrFile != nil {
		closeErr := rcd.startupStdErrFile.Close()
		if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "failed to close startup standard error file")
		}
	}
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

	if rcd.startupStdOutFile != nil && ctr.Status.StartupStdOutFile == "" {
		ctr.Status.StartupStdOutFile = rcd.startupStdOutFile.Name()
		change |= statusChanged
	}
	if rcd.startupStdErrFile != nil && ctr.Status.StartupStdErrFile == "" {
		ctr.Status.StartupStdErrFile = rcd.startupStdErrFile.Name()
		change |= statusChanged
	}

	// We only overwrite the startup timestamp if it is not already set, to avoid problems with rounding errors.
	if !rcd.startAttemptFinishedAt.IsZero() && ctr.Status.StartupTimestamp.IsZero() {
		ctr.Status.StartupTimestamp = rcd.startAttemptFinishedAt
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

	if ctr.Status.FinishTimestamp.IsZero() && rcd.containerState == apiv1.ContainerStateExited {
		ctr.Status.FinishTimestamp = metav1.Now()
		change |= statusChanged
	}

	return change
}
