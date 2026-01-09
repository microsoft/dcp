/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_maps "github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	usvc_slices "github.com/microsoft/dcp/pkg/slices"
)

type resourceHarvester struct {
	// This is a cache of DCP processes that are currently running.
	// It is used to avoid querying the process multiple times.
	processes map[process.Pid_t]bool

	// Map of protected networks that should not be harvested.
	protectedNetworks map[string]bool

	// Synchronization for network harvesting.
	networkHarvestLock *concurrency.ContextAwareLock

	// Has the harvester started? Ensures that we only run the harvester once.
	started atomic.Bool

	// Has the harvester completed? Used to check if harvesting is complete.
	done atomic.Bool
}

func NewResourceHarvester() *resourceHarvester {
	return &resourceHarvester{
		processes:          make(map[process.Pid_t]bool),
		protectedNetworks:  make(map[string]bool),
		networkHarvestLock: concurrency.NewContextAwareLock(),
		started:            atomic.Bool{},
		done:               atomic.Bool{},
	}
}

// This is a mock function to allow for testing controller interactions with the harvester
// without actually harvesting resources. It simply waits a bit and then sets the done flag
// to true.
func (rh *resourceHarvester) MockHarvest(
	ctx context.Context,
	sleepDuration time.Duration,
	log logr.Logger,
) {
	select {
	case <-time.After(sleepDuration):
		log.V(1).Info("Mock resource harvester completed")
		rh.done.Store(true)
		return
	case <-ctx.Done():
		log.V(1).Info("Mock resource harvester cancelled")
		return
	}
}

func (rh *resourceHarvester) TryProtectNetwork(
	ctx context.Context,
	networkName string,
) bool {
	// Harvesting is already done, so nothing further will be harvested.
	if rh.IsDone() {
		return true
	}

	if !rh.networkHarvestLock.TryLock() {
		// If we cannot acquire the lock, the harvester is currently removing networks
		return false
	}
	defer rh.networkHarvestLock.Unlock()

	rh.protectedNetworks[networkName] = true
	return true
}

// This is a separate, public function to allow for easy testing.
func (rh *resourceHarvester) Harvest(
	ctx context.Context,
	co containers.ContainerOrchestrator,
	log logr.Logger,
) {
	// Even if we encounter errors, we are going to log them at Info level.
	// Unused network harvesting is a best-effort operation,
	// and the end user is not expected to take any action if an error occurs during the process.

	// Do not let a panic in the harvester goroutine bring down the entire process.
	defer func() { _ = resiliency.MakePanicError(recover(), log) }()

	if rh.started.Swap(true) {
		// We're already harvesting, so return early.
		return
	}

	defer rh.done.Store(true)

	log.V(1).Info("Unused container resource harvester started...")

	harvestedContainers, harvestErr := rh.harvestAbandonedContainers(ctx, co, log)
	if harvestErr != nil {
		log.Info("Could not harvest all abandoned containers", "Error", harvestErr)
	}

	harvestErr = rh.harvestAbandonedNetworks(ctx, co, harvestedContainers, log)
	if harvestErr != nil {
		log.Info("Could not harvest all abandoned container networks", "Error", harvestErr)
	}
}

func (rh *resourceHarvester) IsDone() bool {
	return rh.done.Load()
}

func (rh *resourceHarvester) harvestAbandonedContainers(
	ctx context.Context,
	co containers.ContainerOrchestrator,
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
	containersToHarvest := usvc_slices.Accumulate[[]string](runningContainers, func(ids []string, c containers.ListedContainer) []string {
		if !nonPersistentWithCreator(c.Labels) {
			// This is a persistent container or missing creator labels, so skip it.
			return ids
		}

		if rh.creatorStillRunning(c.Labels) {
			// The creator process is still running, so skip this container.
			return ids
		}

		return append(ids, c.Id)
	})

	if len(containersToHarvest) == 0 {
		// No containers to harvest, so return early
		return nil, nil
	}

	removedContainerIds, removeErr := co.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: containersToHarvest,
		Force:      true,
	})
	if removeErr != nil && !errors.Is(removeErr, containers.ErrNotFound) {
		// Some containers could not be removed, but we still want to continue harvesting networks.
		// Log the error at Info level, but do not return it.
		log.Info("Could not remove some abandoned containers.", "Error", removeErr)
	}

	log.V(1).Info("Removed containers", "Containers", removedContainerIds)

	return usvc_maps.SliceToMap(removedContainerIds, func(id string) (string, bool) { return id, true }), nil
}

func (rh *resourceHarvester) harvestAbandonedNetworks(
	ctx context.Context,
	co containers.ContainerOrchestrator,
	removedContainers map[string]bool,
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
	networksWithoutCreators := usvc_slices.Accumulate[[]string](runningNetworks, func(ids []string, n containers.ListedNetwork) []string {
		if !withCreator(n.Labels) {
			// This network is missing the required creator labels, so skip it.
			return ids
		}

		if rh.creatorStillRunning(n.Labels) {
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
		log.Info("Could not inspect all running networks", "Error", inspectErr)
	}

	lockErr := rh.networkHarvestLock.Lock(ctx)
	if lockErr != nil {
		log.Info("Could not acquire network harvest lock, skipping network harvesting", "Error", lockErr)
		return lockErr // We could not acquire the lock, so we cannot harvest networks.
	}
	defer rh.networkHarvestLock.Unlock()

	var networksToRemove []string
	for _, network := range inspectedNetworks {
		if _, found := rh.protectedNetworks[network.Name]; found {
			// Skip protected networks (the network controller wants to re-use them).
			continue
		}

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

	if len(networksToRemove) == 0 {
		// No networks to harvest, so return early
		return nil
	}

	removed, removeErr := co.RemoveNetworks(ctx, containers.RemoveNetworksOptions{
		Networks: networksToRemove,
		Force:    true,
	})

	log.V(1).Info("Removed networks", "Networks", removed)

	return removeErr
}

func (rh *resourceHarvester) isRunningDCPProcess(pid process.Pid_t, startTime time.Time) bool {
	if running, exists := rh.processes[pid]; exists {
		return running
	}

	// If the process is not in the cache, we need to check if it is running.
	_, findErr := process.FindProcess(pid, startTime)
	if findErr != nil {
		return false // Process not found, so it's not running.
	}

	// We found the process, so cache it as running.
	rh.processes[pid] = true
	return true
}

// Returns true if the set of given labels belongs to an object (network or container) that was created
// by a DCP process that is still running.
func (rh *resourceHarvester) creatorStillRunning(labels map[string]string) bool {
	creatorPID, _ := process.StringToPidT(labels[CreatorProcessIdLabel])
	creatorStartTime, _ := time.Parse(osutil.RFC3339MiliTimestampFormat, labels[CreatorProcessStartTimeLabel])

	return rh.isRunningDCPProcess(creatorPID, creatorStartTime)
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
