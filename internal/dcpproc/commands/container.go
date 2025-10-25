// Copyright (c) Microsoft Corporation. All rights reserved.

package commands

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	container_runtimes "github.com/microsoft/usvc-apiserver/internal/containers/runtimes"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type inspectStopRemoveContainers interface {
	containers.InspectContainers
	containers.StopContainers
	containers.RemoveContainers
}

const (
	// Use the same timeouts as the Container controller for graceful stop/remove
	stopContainerTimeoutSeconds = 10

	// How often we check whether the container has been removed
	defaultContainerPollInterval = 30 * time.Second
)

var (
	// Container-specific flags
	containerID           string
	containerPollInterval time.Duration
)

func NewContainerCommand(log logr.Logger) (*cobra.Command, error) {
	containerCmd := &cobra.Command{
		Use:   "container",
		Short: "Ensures that a container is stopped and removed when the monitored process exits",
		Long: `Ensures that a container is stopped and removed when the monitored process exits.

This command is used to ensure that containers are properly cleaned up when
DCP terminates unexpectedly. It performs a graceful stop followed by removal of the container.`,
		RunE:         monitorContainer(log),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}

	containerCmd.Flags().StringVar(&containerID, "containerID", "", "The container ID or name to monitor and clean up when DCP exits")
	flagErr := containerCmd.MarkFlagRequired("containerID")
	if flagErr != nil {
		return nil, flagErr // Should never happen--the only error would be if the flag was not found
	}

	// Mostly for testing purposes.
	containerCmd.Flags().DurationVar(&containerPollInterval, "containerPollInterval", defaultContainerPollInterval,
		"How often to poll the container status to check if it has been removed. Default is 30 seconds.")
	flagErr = containerCmd.Flags().MarkHidden("containerPollInterval")
	if flagErr != nil {
		return nil, flagErr // Should never happen
	}

	// Add container runtime flag
	container_flags.EnsureRuntimeFlag(containerCmd.Flags())

	// Add test container orchestrator socket flag (for testing)
	container_flags.EnsureTestContainerOrchestratorSocketFlag(containerCmd.Flags())

	return containerCmd, nil
}

func monitorContainer(log logr.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if containerID == "" {
			return errors.New("container ID or name must be specified with --container flag")
		}

		log = log.WithName("ContainerMonitor").
			WithValues(
				"MonitorPID", monitorPid,
				"Container", containerID,
			)

		if resourceId != "" {
			log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resourceId)
		}

		co, pe, coErr := getContainerOrchestrator(cmd.Context(), log)
		if coErr != nil {
			log.Error(coErr, "Unable to ensure container cleanup")
			return coErr
		}
		defer pe.Dispose()

		monitorCtx, monitorCtxCancel, monitorCtxErr := cmds.MonitorPid(cmd.Context(), monitorPid, monitorProcessStartTime, monitorInterval, log)
		defer monitorCtxCancel()
		if monitorCtxErr != nil {
			if errors.Is(monitorCtxErr, os.ErrProcessDone) {
				// If the monitor process is already terminated, cleanup the container immediately
				log.Info("Monitored process already exited, cleaning up container")
				return doCleanupContainer(cmd.Context(), containerID, log, co)
			} else {
				log.Error(monitorCtxErr, "Process could not be monitored")
				return monitorCtxErr
			}
		}

		ctrRemovedCh := pollContainerRemoved(monitorCtx, containerID, co, log)

		select {
		case <-ctrRemovedCh:
			// Container was removed, we are done
			return nil
		case <-monitorCtx.Done():
			log.Info("Monitored process exited, cleaning up container")
			return doCleanupContainer(cmd.Context(), containerID, log, co)
		}
	}
}

func getContainerOrchestrator(ctx context.Context, log logr.Logger) (inspectStopRemoveContainers, process.Executor, error) {
	pe := process.NewOSExecutor(log.WithName("ProcessExecutor"))

	co := container_flags.TryGetRemoteContainerOrchestrator(ctx, log.WithName("RemoteContainerOrchestrator"))
	if co == nil {
		sco, scoErr := container_runtimes.FindAvailableContainerRuntime(
			ctx,
			log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagValue()),
			pe,
		)
		if scoErr != nil {
			log.Error(scoErr, "Failed to find available container runtime")
			return nil, nil, scoErr
		}
		co = sco
	}

	return co, pe, nil
}

func doCleanupContainer(
	ctx context.Context,
	containerID string,
	log logr.Logger,
	co inspectStopRemoveContainers,
) error {
	// Check if the container still exists
	inspected, inspectErr := co.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{containerID},
	})

	if inspectErr != nil {
		if errors.Is(inspectErr, containers.ErrNotFound) {
			// Great, the container is already gone, nothing to clean up
			return nil
		}

		existenceCheckErr := fmt.Errorf("unable to check whether container '%s' still exists: %w", containerID, inspectErr)
		log.Error(existenceCheckErr, "Container (if exists) will not be cleaned up")
		return inspectErr
	}

	if len(inspected) == 0 {
		log.Info("Container inspection returned no errors, and no result... (?)") // Should never happen
		return nil
	}

	// Typically the container should be cleaned up by the monitored process, so if it is still running,
	// this is somewhat unexpected and worth logging.
	log.Info("Found existing container, cleaning it up...")

	container := inspected[0]
	shouldStop := container.Status == containers.ContainerStatusRunning ||
		container.Status == containers.ContainerStatusPaused ||
		container.Status == containers.ContainerStatusRestarting

	if shouldStop {
		_, stopErr := co.StopContainers(ctx, containers.StopContainersOptions{
			Containers:    []string{containerID},
			SecondsToKill: stopContainerTimeoutSeconds,
		})

		if stopErr != nil && !errors.Is(stopErr, containers.ErrNotFound) {
			log.Error(stopErr, "Failed to stop container gracefully")
			// Continue with removal even if stop failed
		}
	}

	// Remove the container
	_, removeErr := co.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{containerID},
		Force:      true,
	})

	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		log.Error(removeErr, "Failed to remove container")
		return removeErr
	}

	return nil
}

func pollContainerRemoved(ctx context.Context, containerID string, co inspectStopRemoveContainers, log logr.Logger) <-chan struct{} {
	ctrRemovedCh := make(chan struct{})

	jitter := func() time.Duration {
		// Up to 5% of the poll interval, to avoid all instances of dcpproc polling at the same exact time
		return time.Duration(rand.Int63n(int64(containerPollInterval / 20.0)))
	}

	go func() {
		defer close(ctrRemovedCh)
		// Use the configured poll interval (overridable via hidden flag for tests)
		timer := time.NewTimer(containerPollInterval + jitter())
		defer timer.Stop()

		for {
			select {

			case <-ctx.Done():
				return

			case <-timer.C:
				// Poll the container status
				_, inspectErr := co.InspectContainers(ctx, containers.InspectContainersOptions{
					Containers: []string{containerID},
				})

				if inspectErr != nil {
					if errors.Is(inspectErr, containers.ErrNotFound) {
						// Container has been removed, we should exit, which will close the channel
						// and notify the caller.
						return
					} else {
						log.Error(inspectErr, "Failed to inspect container")
						// May be transient error, continue polling
					}
				}

				timer.Reset(containerPollInterval + jitter())
			}
		}
	}()

	return ctrRemovedCh
}
