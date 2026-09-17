/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	container_runtimes "github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	defaultVolumePollInterval             = 30 * time.Second
	volumeCleanupRetryInitialInterval     = 500 * time.Millisecond
	volumeCleanupRetryMaxInterval         = 5 * time.Second
	volumeCleanupRetryTimeout             = 30 * time.Second
	volumeCleanupRetryRandomizationFactor = 0.1
	volumeCleanupRetryBackoffMultiplier   = 2.0
)

var (
	volumeID           string
	volumeResourceUID  string
	volumePollInterval time.Duration
)

func NewVolumeCommand(log logr.Logger) (*cobra.Command, error) {
	volumeCmd := &cobra.Command{
		Use:   "monitor-container-volume",
		Short: "Ensures that a container volume is removed when the monitored process exits",
		Long: `Ensures that a container volume is removed when the monitored process exits.

This command is used to ensure that container volumes are properly cleaned up when
DCP terminates unexpectedly. Volumes are never force-removed.`,
		RunE:         monitorVolume(log),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}

	flagErr := addMonitorFlags(volumeCmd)
	if flagErr != nil {
		return nil, flagErr
	}

	volumeCmd.Flags().StringVar(&volumeID, "volumeID", "", "The volume ID or name to monitor and clean up when DCP exits")
	flagErr = volumeCmd.MarkFlagRequired("volumeID")
	if flagErr != nil {
		return nil, flagErr
	}

	volumeCmd.Flags().StringVar(&volumeResourceUID, "resourceUID", "", "The UID of the PhysicalContainerVolume that created the volume")
	flagErr = volumeCmd.MarkFlagRequired("resourceUID")
	if flagErr != nil {
		return nil, flagErr
	}

	volumeCmd.Flags().DurationVar(
		&volumePollInterval,
		"volumePollInterval",
		defaultVolumePollInterval,
		"How often to poll the volume status to check if it has been removed. Default is 30 seconds.",
	)
	flagErr = volumeCmd.Flags().MarkHidden("volumePollInterval")
	if flagErr != nil {
		return nil, flagErr
	}

	container_flags.EnsureRuntimeFlag(volumeCmd.Flags())

	return volumeCmd, nil
}

func monitorVolume(log logr.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if volumeID == "" {
			return errors.New("volume ID or name must be specified with --volumeID")
		}
		if volumeResourceUID == "" {
			return errors.New("physical container volume UID must be specified with --resourceUID")
		}

		log = log.WithName("ContainerVolumeMonitor").
			WithValues(
				"MonitorPID", monitorPid,
				"Volume", volumeID,
				"ResourceUID", volumeResourceUID,
			)
		if resourceId != "" {
			log = log.WithValues(logger.RESOURCE_LOG_STREAM_ID, resourceId)
		}

		processExecutor := process.NewOSExecutor(log.WithName("ProcessExecutor"))
		defer processExecutor.Dispose()
		orchestrator, orchestratorErr := container_runtimes.FindAvailableContainerRuntime(
			cmd.Context(),
			log.WithName("ContainerOrchestrator").WithValues("ContainerRuntime", container_flags.GetRuntimeFlagValue()),
			processExecutor,
		)
		if orchestratorErr != nil {
			log.Error(orchestratorErr, "Unable to ensure container volume cleanup")
			return orchestratorErr
		}

		monitorCtx, monitorCtxCancel, monitorCtxErr := cmds.MonitorPid(
			cmd.Context(),
			process.NewHandle(monitorPid, monitorProcessStartTime),
			monitorInterval,
			log,
		)
		defer monitorCtxCancel()
		if monitorCtxErr != nil {
			if isMonitorProcessGoneErr(monitorCtxErr) {
				log.Info("Monitored process already exited, cleaning up container volume", "Reason", monitorCtxErr)
				return cleanupVolumeAfterMonitorExit(
					cmd.Context(),
					volumeID,
					volumeResourceUID,
					newVolumeCleanupBackoff(),
					log,
					orchestrator,
				)
			}

			log.Error(monitorCtxErr, "Process could not be monitored")
			return monitorCtxErr
		}

		if pollVolumeRemoved(monitorCtx, volumeID, volumeResourceUID, orchestrator, log) {
			return nil
		}

		log.Info("Monitored process exited, cleaning up container volume")
		return cleanupVolumeAfterMonitorExit(
			cmd.Context(),
			volumeID,
			volumeResourceUID,
			newVolumeCleanupBackoff(),
			log,
			orchestrator,
		)
	}
}

func cleanupVolumeAfterMonitorExit(
	ctx context.Context,
	volumeID string,
	resourceUID string,
	retryPolicy backoff.BackOff,
	log logr.Logger,
	orchestrator containers.VolumeOrchestrator,
) error {
	waitingForContainerCleanup := false
	return resiliency.Retry(ctx, retryPolicy, func() error {
		cleanupErr := doCleanupVolume(ctx, volumeID, resourceUID, orchestrator)
		if cleanupErr == nil {
			return nil
		}
		if !errors.Is(cleanupErr, containers.ErrObjectInUse) {
			return resiliency.Permanent(cleanupErr)
		}

		if !waitingForContainerCleanup {
			log.Info(
				"Container volume is still in use; waiting for container cleanup before retrying removal",
				"Timeout",
				volumeCleanupRetryTimeout,
			)
			waitingForContainerCleanup = true
		} else {
			log.V(1).Info("Container volume is still in use; retrying after backoff")
		}
		return cleanupErr
	})
}

func newVolumeCleanupBackoff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(volumeCleanupRetryInitialInterval),
		backoff.WithMaxInterval(volumeCleanupRetryMaxInterval),
		backoff.WithMaxElapsedTime(volumeCleanupRetryTimeout),
		backoff.WithRandomizationFactor(volumeCleanupRetryRandomizationFactor),
		backoff.WithMultiplier(volumeCleanupRetryBackoffMultiplier),
	)
}

func doCleanupVolume(
	ctx context.Context,
	volumeID string,
	resourceUID string,
	orchestrator containers.VolumeOrchestrator,
) error {
	inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{volumeID},
	})
	if inspectErr != nil && !errors.Is(inspectErr, containers.ErrIncomplete) {
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("inspect container volume before removal: %w", inspectErr)
	}
	if len(inspectedVolumes) == 0 {
		return nil
	}
	if inspectedVolumes[0].Labels[containers.ResourceUIDLabel] != resourceUID {
		return nil
	}

	_, removeErr := orchestrator.RemoveVolumes(ctx, containers.RemoveVolumesOptions{
		Volumes: []string{volumeID},
		Force:   false,
	})
	if removeErr == nil {
		return nil
	}

	_, confirmErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
		Volumes: []string{volumeID},
	})
	if errors.Is(confirmErr, containers.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("remove container volume: %w", errors.Join(removeErr, confirmErr))
}

func pollVolumeRemoved(
	ctx context.Context,
	volumeID string,
	resourceUID string,
	orchestrator containers.InspectVolumes,
	log logr.Logger,
) bool {
	return pollContainerResourceRemoved(
		ctx,
		volumePollInterval,
		func(ctx context.Context) (bool, error) {
			inspectedVolumes, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
				Volumes: []string{volumeID},
			})
			if errors.Is(inspectErr, containers.ErrNotFound) || (inspectErr == nil && len(inspectedVolumes) == 0) {
				return true, nil
			}
			if len(inspectedVolumes) > 0 && inspectedVolumes[0].Labels[containers.ResourceUIDLabel] != resourceUID {
				return true, nil
			}
			if inspectErr != nil && !errors.Is(inspectErr, containers.ErrIncomplete) {
				return false, inspectErr
			}
			return false, nil
		},
		"Failed to inspect container volume",
		log,
	)
}
