/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/ports"
)

// PortReservationSet retains an owner's port claims until Release is called.
// The zero value is usable. Sets must not be copied after first use.
type PortReservationSet struct {
	bindings map[portReservationKey]struct{}
}

type portReservationKey struct {
	binding ports.Binding
	config  *stateStorePortAllocationConfig // nil identifies the MRU backend.
}

var portReservations = struct {
	sync.Mutex
	owners map[portReservationKey]map[*PortReservationSet]struct{}
}{
	owners: make(map[portReservationKey]map[*PortReservationSet]struct{}),
}

// GetFreePort allocates a port and retains its exact binding and allocation backend for release.
func (reservations *PortReservationSet) GetFreePort(
	ctx context.Context, protocol commonapi.PortProtocol, address string, log logr.Logger,
) (int32, error) {
	normalizedAddress, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return 0, addressErr
	}
	ip, parseErr := netip.ParseAddr(ToStandaloneAddress(normalizedAddress))
	if parseErr != nil {
		return 0, parseErr
	}
	portReservations.Lock()
	defer portReservations.Unlock()
	port, config, fallback, allocationErr := allocatePortFromStateStoreRange(ctx, protocol, normalizedAddress, log)
	if allocationErr != nil && fallback {
		log.V(1).Info("State store port allocation unavailable, using MRU allocation", "Error", allocationErr)
		port, allocationErr = allocateRandomEphemeralPortWithMruFile(ctx, protocol, normalizedAddress, log)
	}
	if allocationErr != nil {
		return 0, allocationErr
	}
	reservations.retain(portReservationKey{
		binding: ports.Binding{Protocol: string(protocol), IP: ip.Unmap(), Port: port}, config: config,
	})
	return port, nil
}

// Reserve retains a declared binding without requiring it to be currently bindable.
func (reservations *PortReservationSet) Reserve(ctx context.Context, binding ports.Binding, log logr.Logger) error {
	binding.IP = binding.IP.Unmap()
	binding.Protocol = strings.ToUpper(strings.TrimSpace(binding.Protocol))
	if !binding.IP.IsValid() || !IsValidPort(int(binding.Port)) {
		return fmt.Errorf("invalid port reservation binding: %s/%s/%d", binding.Protocol, binding.IP, binding.Port)
	}
	portReservations.Lock()
	defer portReservations.Unlock()
	config, fallback, reserveErr := reserveSpecificStateStorePort(ctx, binding, "", false, log)
	if reserveErr != nil {
		if !fallback {
			return reserveErr
		}
		log.V(1).Info("State store port reservation unavailable, retaining legacy allocation behavior", "Error", reserveErr)
		return nil // Declared ports do not create MRU records.
	}
	if config != nil {
		reservations.retain(portReservationKey{binding: binding, config: config})
	}
	return nil
}

// CheckPortAvailable checks bindability and retains the binding if available.
func (reservations *PortReservationSet) CheckPortAvailable(
	ctx context.Context, protocol commonapi.PortProtocol, address string, port int32, log logr.Logger,
) error {
	normalizedAddress, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return addressErr
	}
	ip, parseErr := netip.ParseAddr(ToStandaloneAddress(normalizedAddress))
	if parseErr != nil {
		return parseErr
	}
	portReservations.Lock()
	defer portReservations.Unlock()
	config, fallback, checkErr := checkPortAvailableWithStateStore(ctx, protocol, normalizedAddress, port, log)
	if checkErr != nil {
		if !fallback {
			return checkErr
		}
		log.V(1).Info("State store port check unavailable, using MRU availability check", "Error", checkErr)
		return checkPortAvailableWithMruFile(ctx, protocol, normalizedAddress, port, log)
	}
	if config != nil {
		reservations.retain(portReservationKey{
			binding: ports.Binding{Protocol: string(protocol), IP: ip.Unmap(), Port: port}, config: config,
		})
	}
	return nil
}

// Release relinquishes this owner's claims without retrying failed backend releases.
// Claims shared with another owner remain reserved.
func (reservations *PortReservationSet) Release(ctx context.Context, log logr.Logger) {
	if reservations == nil {
		return
	}
	portReservations.Lock()
	defer portReservations.Unlock()
	for key := range reservations.bindings {
		owners := portReservations.owners[key]
		delete(owners, reservations)
		if len(owners) == 0 {
			delete(portReservations.owners, key)
			var releaseErr error
			if key.config == nil {
				releaseErr = releaseSpecificPortWithMruFile(ctx, addrToAddressString(key.binding.IP), key.binding.Port, log)
			} else {
				releaseCtx, cancel := context.WithTimeout(ctx, key.config.allocationTimeout)
				releaseErr = key.config.store.ReleasePort(releaseCtx, stateStorePortReservationRequest(key.binding, key.config.owner))
				cancel()
			}
			if releaseErr != nil {
				log.Error(releaseErr, "Could not release port reservation", "Binding", key.binding)
			}
		}
	}
	reservations.bindings = nil
}

func (reservations *PortReservationSet) retain(key portReservationKey) {
	if reservations.bindings == nil {
		reservations.bindings = make(map[portReservationKey]struct{})
	}
	reservations.bindings[key] = struct{}{}
	if portReservations.owners[key] == nil {
		portReservations.owners[key] = make(map[*PortReservationSet]struct{})
	}
	portReservations.owners[key][reservations] = struct{}{}
}
