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

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	container_runtimes "github.com/microsoft/dcp/internal/containers/runtimes"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/process"
)

const defaultNetworkPollInterval = 30 * time.Second

var (
	networkID           string
	networkPollInterval time.Duration
)

func NewNetworkCommand(log logr.Logger) (*cobra.Command, error) {
	networkCmd := &cobra.Command{
		Use:   "monitor-container-network",
		Short: "Ensures that a container network is removed when the monitored process exits",
		Long: `Ensures that a container network is removed when the monitored process exits.

This command is used to ensure that container networks are properly cleaned up when
DCP terminates unexpectedly. Attached containers are disconnected without being removed.`,
		RunE:         monitorNetwork(log),
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}

	flagErr := addMonitorFlags(networkCmd)
	if flagErr != nil {
		return nil, flagErr
	}

	networkCmd.Flags().StringVar(&networkID, "networkID", "", "The network ID or name to monitor and clean up when DCP exits")
	flagErr = networkCmd.MarkFlagRequired("networkID")
	if flagErr != nil {
		return nil, flagErr
	}

	networkCmd.Flags().DurationVar(
		&networkPollInterval,
		"networkPollInterval",
		defaultNetworkPollInterval,
		"How often to poll the network status to check if it has been removed. Default is 30 seconds.",
	)
	flagErr = networkCmd.Flags().MarkHidden("networkPollInterval")
	if flagErr != nil {
		return nil, flagErr
	}

	container_flags.EnsureRuntimeFlag(networkCmd.Flags())

	return networkCmd, nil
}

func monitorNetwork(log logr.Logger) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if networkID == "" {
			return errors.New("network ID or name must be specified with --networkID")
		}

		log = log.WithName("ContainerNetworkMonitor").
			WithValues(
				"MonitorPID", monitorPid,
				"Network", networkID,
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
			log.Error(orchestratorErr, "Unable to ensure container network cleanup")
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
				log.Info("Monitored process already exited, cleaning up container network", "Reason", monitorCtxErr)
				return doCleanupNetwork(cmd.Context(), networkID, log, orchestrator)
			}

			log.Error(monitorCtxErr, "Process could not be monitored")
			return monitorCtxErr
		}

		if pollNetworkRemoved(monitorCtx, networkID, orchestrator, log) {
			return nil
		}

		log.Info("Monitored process exited, cleaning up container network")
		return doCleanupNetwork(cmd.Context(), networkID, log, orchestrator)
	}
}

func doCleanupNetwork(
	ctx context.Context,
	networkID string,
	log logr.Logger,
	orchestrator containers.NetworkAttachmentOrchestrator,
) error {
	inspectedNetworks, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{networkID},
	})
	if inspectErr != nil && !errors.Is(inspectErr, containers.ErrIncomplete) {
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("inspect container network before removal: %w", inspectErr)
	}
	if len(inspectedNetworks) == 0 {
		return nil
	}

	network := inspectedNetworks[0]
	if orchestrator.IsBuiltInNetwork(network.Name) {
		log.Info("Skipping cleanup of built-in container network", "NetworkName", network.Name)
		return nil
	}

	listedContainers, listErr := orchestrator.ListContainers(ctx, containers.ListContainersOptions{
		All: true,
		Filters: containers.ListContainersFilters{
			NetworkFilters: []string{network.Id},
		},
	})
	if listErr != nil {
		_, confirmErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{networkID},
		})
		if errors.Is(confirmErr, containers.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("list containers attached to container network: %w", errors.Join(listErr, confirmErr))
	}

	attachedContainerIDs := make(map[string]struct{}, len(network.Containers)+len(listedContainers))
	for _, attachedContainer := range network.Containers {
		attachedContainerIDs[attachedContainer.Id] = struct{}{}
	}
	for _, listedContainer := range listedContainers {
		attachedContainerIDs[listedContainer.Id] = struct{}{}
	}

	var disconnectErrors error
	for containerID := range attachedContainerIDs {
		disconnectErr := orchestrator.DisconnectNetwork(ctx, containers.DisconnectNetworkOptions{
			Network:   network.Id,
			Container: containerID,
			Force:     true,
		})
		if disconnectErr != nil && !errors.Is(disconnectErr, containers.ErrNotFound) {
			disconnectErrors = errors.Join(disconnectErrors, disconnectErr)
		}
	}
	if disconnectErrors != nil {
		return fmt.Errorf("disconnect all containers from container network: %w", disconnectErrors)
	}

	_, removeErr := orchestrator.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: []string{networkID},
	})
	if removeErr == nil {
		return nil
	}

	_, confirmErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{networkID},
	})
	if errors.Is(confirmErr, containers.ErrNotFound) {
		return nil
	}
	return fmt.Errorf("remove container network: %w", errors.Join(removeErr, confirmErr))
}

func pollNetworkRemoved(
	ctx context.Context,
	networkID string,
	orchestrator containers.InspectNetworks,
	log logr.Logger,
) bool {
	return pollContainerResourceRemoved(
		ctx,
		networkPollInterval,
		func(ctx context.Context) (bool, error) {
			inspectedNetworks, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
				Networks: []string{networkID},
			})
			if errors.Is(inspectErr, containers.ErrNotFound) || (inspectErr == nil && len(inspectedNetworks) == 0) {
				return true, nil
			}
			if inspectErr != nil && !errors.Is(inspectErr, containers.ErrIncomplete) {
				return false, inspectErr
			}
			return false, nil
		},
		"Failed to inspect container network",
		log,
	)
}
