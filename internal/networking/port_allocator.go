/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"
	"fmt"
	std_slices "slices"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Gets a free TCP or UDP port for a given address (defaults to localhost).
// Even if this method is called twice in a row, it should not return the same port.
func GetFreePort(ctx context.Context, protocol commonapi.PortProtocol, address string, log logr.Logger) (int32, error) {
	protocol = NormalizePortProtocol(protocol)
	normalizedAddress, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return 0, addressErr
	}
	probeAddresses, probeErr := portProbeAddresses(address, normalizedAddress)
	if probeErr != nil {
		return 0, probeErr
	}

	stateStorePort, _, shouldFallback, stateStoreErr := allocatePortFromStateStoreRange(ctx, protocol, normalizedAddress, probeAddresses, log)
	if stateStoreErr == nil {
		return stateStorePort, nil
	}
	if !shouldFallback {
		return 0, stateStoreErr
	}
	log.V(1).Info("Warning: state store port allocation failed, falling back to MRU port allocation", "Error", stateStoreErr.Error())
	return allocateRandomEphemeralPortWithMruFile(ctx, protocol, normalizedAddress, probeAddresses, log)
}

func CheckPortAvailable(ctx context.Context, protocol commonapi.PortProtocol, address string, port int32, log logr.Logger) error {
	protocol = NormalizePortProtocol(protocol)
	normalizedAddress, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return addressErr
	}
	probeAddresses, probeErr := portProbeAddresses(address, normalizedAddress)
	if probeErr != nil {
		return probeErr
	}

	_, shouldFallback, stateStoreErr := checkPortAvailableWithStateStore(ctx, protocol, normalizedAddress, probeAddresses, port, log)
	if stateStoreErr == nil {
		return nil
	}
	if !shouldFallback {
		return stateStoreErr
	}
	log.V(1).Info("Warning: state store port availability check failed, falling back to MRU port availability check", "Error", stateStoreErr.Error())
	return checkPortAvailableWithMruFile(ctx, protocol, normalizedAddress, probeAddresses, port, log)
}

// portProbeAddresses returns every address that must be bindable for a requested allocation address.
// Localhost requests must be free on all loopback addresses because listeners commonly bind each of them.
func portProbeAddresses(requestedAddress string, normalizedAddress string) ([]string, error) {
	if requestedAddress != "" && requestedAddress != Localhost {
		return []string{normalizedAddress}, nil
	}
	loopbackIps, lookupErr := LookupIP(Localhost)
	if lookupErr != nil {
		return nil, fmt.Errorf("could not resolve localhost addresses for port probing: %w", lookupErr)
	}
	probeAddresses := []string{normalizedAddress}
	for _, ip := range loopbackIps {
		address := IpToString(ip)
		if !std_slices.Contains(probeAddresses, address) {
			probeAddresses = append(probeAddresses, address)
		}
	}
	return probeAddresses, nil
}
