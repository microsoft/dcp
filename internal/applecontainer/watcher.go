/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"context"
	"encoding/json"
	"time"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
)

// The Apple container CLI has no `events` command, so container and network events
// are synthesized by polling the runtime and diffing consecutive snapshots.
// Events are delivered on a best-effort basis (same as for Docker/Podman, where the
// `events` stream is not restarted if it fails).

const (
	containerPollInterval = 2 * time.Second
	networkPollInterval   = 5 * time.Second
)

func (aco *AppleContainerCliOrchestrator) doWatchContainers(watcherCtx context.Context, ss *pubsub.SubscriptionSet[containers.EventMessage]) {
	knownStates, _ := aco.snapshotContainerStates(watcherCtx)

	timer := time.NewTicker(containerPollInterval)
	defer timer.Stop()

	for {
		select {
		case <-watcherCtx.Done():
			return
		case <-timer.C:
		}

		newStates, err := aco.snapshotContainerStates(watcherCtx)
		if err != nil {
			if watcherCtx.Err() == nil {
				aco.log.V(1).Info("Polling for container events failed", "Error", err.Error())
			}
			continue
		}

		for id, state := range newStates {
			oldState, known := knownStates[id]
			switch {
			case !known:
				ss.Notify(containerEvent(containers.EventActionCreate, id))
				if state == "running" {
					ss.Notify(containerEvent(containers.EventActionStart, id))
				}
			case oldState != "running" && state == "running":
				ss.Notify(containerEvent(containers.EventActionStart, id))
			case oldState == "running" && state != "running":
				ss.Notify(containerEvent(containers.EventActionDie, id))
			}
		}

		for id := range knownStates {
			if _, stillExists := newStates[id]; !stillExists {
				ss.Notify(containerEvent(containers.EventActionDestroy, id))
			}
		}

		knownStates = newStates
	}
}

func (aco *AppleContainerCliOrchestrator) doWatchNetworks(watcherCtx context.Context, ss *pubsub.SubscriptionSet[containers.EventMessage]) {
	knownNetworks, _ := aco.snapshotNetworks(watcherCtx)

	timer := time.NewTicker(networkPollInterval)
	defer timer.Stop()

	for {
		select {
		case <-watcherCtx.Done():
			return
		case <-timer.C:
		}

		newNetworks, err := aco.snapshotNetworks(watcherCtx)
		if err != nil {
			if watcherCtx.Err() == nil {
				aco.log.V(1).Info("Polling for network events failed", "Error", err.Error())
			}
			continue
		}

		for id := range newNetworks {
			if _, known := knownNetworks[id]; !known {
				ss.Notify(networkEvent(containers.EventActionCreate, id))
			}
		}

		for id := range knownNetworks {
			if _, stillExists := newNetworks[id]; !stillExists {
				ss.Notify(networkEvent(containers.EventActionDestroy, id))
			}
		}

		knownNetworks = newNetworks
	}
}

// snapshotContainerStates returns the current state (e.g. "running", "stopped") of all
// containers known to the runtime, keyed by container ID.
func (aco *AppleContainerCliOrchestrator) snapshotContainerStates(ctx context.Context) (map[string]string, error) {
	cmd := makeAppleContainerCommand("ls", "--all", "--format", "json")
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "WatchContainers", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(errBuf)
	}

	var listed []appleListedContainer
	if unmarshalErr := json.Unmarshal(outBuf.Bytes(), &listed); unmarshalErr != nil {
		return nil, unmarshalErr
	}

	states := make(map[string]string, len(listed))
	for _, ctr := range listed {
		states[ctr.Id] = ctr.Status.State
	}

	return states, nil
}

// snapshotNetworks returns the set of network IDs known to the runtime.
func (aco *AppleContainerCliOrchestrator) snapshotNetworks(ctx context.Context) (map[string]struct{}, error) {
	cmd := makeAppleContainerCommand("network", "ls", "--format", "json")
	outBuf, errBuf, err := aco.runBufferedCommand(ctx, "WatchNetworks", cmd, nil, nil, ordinaryCommandTimeout)
	if err != nil {
		return nil, normalizeCliErrors(errBuf)
	}

	var listed []appleInspectedNetwork
	if unmarshalErr := json.Unmarshal(outBuf.Bytes(), &listed); unmarshalErr != nil {
		return nil, unmarshalErr
	}

	networks := make(map[string]struct{}, len(listed))
	for _, network := range listed {
		networks[network.Id] = struct{}{}
	}

	return networks, nil
}

func containerEvent(action containers.EventAction, containerId string) containers.EventMessage {
	return containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: action,
		Actor:  containers.EventActor{ID: containerId},
	}
}

func networkEvent(action containers.EventAction, networkId string) containers.EventMessage {
	return containers.EventMessage{
		Source: containers.EventSourceNetwork,
		Action: action,
		Actor:  containers.EventActor{ID: networkId},
	}
}
