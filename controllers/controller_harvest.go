package controllers

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	usvc_maps "github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	usvc_slices "github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/tklauser/ps"
)

// This is a separate, public function to allow for easy testing.
func HarvestAbandonedContainerResources(
	ctx context.Context,
	co containers.ContainerOrchestrator,
	log logr.Logger,
) error {
	log.V(1).Info("unused container resource harvester started...")

	// Even if we encounter errors, we are going to log them at Info level.
	// Unused network harvesting is a best-effort operation,
	// and the end user is not expected to take any action if an error occurs during the process.

	// Do not let a panic in the harvester goroutine bring down the entire process.
	defer func() { _ = resiliency.MakePanicError(recover(), log) }()

	procs, procsErr := ps.Processes()
	if procsErr != nil {
		log.Info("Could not list current processes; unused container resources will not be harvested", "Error", procsErr)
		return procsErr
	}

	harvestedContainers, harvestErr := harvestAbandonedContainers(ctx, co, procs, log)
	if harvestErr != nil {
		log.Info("could not harvest all abandoned containers", "Error", harvestErr)
	}

	harvestErr = harvestAbandonedNetworks(ctx, co, harvestedContainers, procs, log)
	if harvestErr != nil {
		log.Info("could not harvest all abandoned container networks", "Error", harvestErr)
	}

	return nil
}

func harvestAbandonedContainers(
	ctx context.Context,
	co containers.ContainerOrchestrator,
	procs []ps.Process,
	log logr.Logger,
) (map[string]bool, error) {
	runningContainers, listErr := co.ListContainers(ctx, containers.ListContainersOptions{
		Filters: containers.ListContainersFilters{
			LabelFilters: []containers.LabelFilter{
				{Key: CreatorProcessIdLabel},
			},
		},
	})
	if listErr != nil {
		return nil, listErr
	}

	if len(runningContainers) == 0 {
		// No containers to harvest, so return early.
		return nil, nil
	}

	// Reduce the list of running containers to the IDs of the ones that are non-persistent and don't have an active creator process.
	containersToHarvest := usvc_slices.Accumulate[containers.ListedContainer, []string](runningContainers, func(ids []string, c containers.ListedContainer) []string {
		if !nonPersistentWithCreator(c.Labels) {
			// This is a persistent container or missing creator labels, so skip it.
			return ids
		}

		if creatorStillRunning(c.Labels, procs) {
			// The creator process is still running, so skip this container.
			return ids
		}

		return append(ids, c.Id)
	})

	removedContainerIds, removeErr := co.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: containersToHarvest,
		Force:      true,
	})
	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		// Some containers could not be removed, but we still want to continue harvesting networks.
		// Log the error at Info level, but do not return it.
		log.Info("could not remove some abandoned containers.", "Error", removeErr)
	}

	return usvc_maps.SliceToMap(removedContainerIds, func(id string) (string, bool) { return id, true }), nil
}

func harvestAbandonedNetworks(
	ctx context.Context,
	co containers.ContainerOrchestrator,
	removedContainers map[string]bool,
	procs []ps.Process,
	log logr.Logger,
) error {
	runningNetworks, listErr := co.ListNetworks(ctx, containers.ListNetworksOptions{
		Filters: containers.ListNetworksFilters{
			LabelFilters: []containers.LabelFilter{
				{Key: CreatorProcessIdLabel},
			},
		},
	})
	if listErr != nil {
		return listErr
	}

	if len(runningNetworks) == 0 {
		// No networks to harvest, so return early.
		return nil
	}

	// Reduce the list of running networks to the ones that are empty and don't have an
	// active creator process.
	networksWithoutCreators := usvc_slices.Accumulate[containers.ListedNetwork, []string](runningNetworks, func(ids []string, n containers.ListedNetwork) []string {
		if !withCreator(n.Labels) {
			// This network is missing the required creator labels, so skip it.
			return ids
		}

		if creatorStillRunning(n.Labels, procs) {
			return ids
		}

		return append(ids, n.ID)
	})

	if len(networksWithoutCreators) == 0 {
		// No networks to harvest, so return early
		return nil
	}

	inspectedNetworks, inspectErr := co.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: networksWithoutCreators,
	})

	if errors.Is(inspectErr, containers.ErrIncomplete) {
		log.Info("could not inspect all running networks", "Error", inspectErr)
	}

	var networksToRemove []string
	for _, network := range inspectedNetworks {
		foundAll := true

		for _, container := range network.Containers {
			// Verify that this isn't a container that was just removed
			if !removedContainers[container.Id] {
				foundAll = false
				break
			}
		}

		if foundAll {
			// There are no running containers connected to this network,
			// so we want to remove it.
			networksToRemove = append(networksToRemove, network.Id)
			continue
		}
	}

	_, removeErr := co.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: networksToRemove,
		Force:    true,
	})

	return removeErr
}

// Checks for the presence of the creator process ID and start time labels.
func withCreator(labels map[string]string) bool {
	_, hasValidPid := usvc_maps.TryGetValidValue(labels, CreatorProcessIdLabel, func(v string) bool {
		_, err := process.StringToPidT(v)
		return err == nil
	})
	_, hasValidStartTime := usvc_maps.TryGetValidValue(labels, CreatorProcessStartTimeLabel, func(v string) bool {
		_, err := time.Parse(osutil.RFC3339MiliTimestampFormat, v)
		return err == nil
	})
	return hasValidPid && hasValidStartTime
}

// Returns true if the set of given labels belongs to an object (network or container) that is non-persistent
// and has a creator process ID/start time label set (and thus was created by DCP).
func nonPersistentWithCreator(labels map[string]string) bool {
	nonPersistent := usvc_maps.HasExactValue(labels, PersistentLabel, "false")
	hasCreator := withCreator(labels)
	return nonPersistent && hasCreator
}

// Returns true if the set of given labels belongs to an object (network or container) that was created
// by a DCP process that is still running.
func creatorStillRunning(labels map[string]string, procs []ps.Process) bool {
	creatorPID, _ := process.StringToPidT(labels[CreatorProcessIdLabel])
	creatorStartTime, _ := time.Parse(osutil.RFC3339MiliTimestampFormat, labels[CreatorProcessStartTimeLabel])

	return usvc_slices.Any(procs, func(p ps.Process) bool {
		pPid, pPidErr := process.IntToPidT(p.PID())
		if pPidErr != nil {
			return true // Can't convert the PID, so assume it's a running process and the network is off limits.
		}
		return pPid == creatorPID && process.HasExpectedStartTime(p, creatorStartTime)
	})
}
