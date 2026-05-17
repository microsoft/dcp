/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

// Gets a free TCP or UDP port for a given address (defaults to localhost).
// Even if this method is called twice in a row, it should not return the same port.
func GetFreePort(ctx context.Context, protocol apiv1.PortProtocol, address string, log logr.Logger) (int32, error) {
	address, addressErr := normalizePortAllocationAddress(address)
	if addressErr != nil {
		return 0, addressErr
	}

	stateStorePort, stateStoreErr, shouldFallback := allocatePortFromStateStoreRange(ctx, protocol, address, log)
	if stateStoreErr == nil {
		return stateStorePort, nil
	}
	if !shouldFallback {
		return 0, stateStoreErr
	}
	if stateStoreErr != nil {
		log.V(1).Info("Warning: state store port allocation failed, falling back to MRU port allocation", "Error", stateStoreErr.Error())
	}
	return allocateRandomEphemeralPortWithMruFile(ctx, protocol, address, log)
}

func CheckPortAvailable(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) error {
	address, addressErr := normalizePortAllocationAddress(address)
	if addressErr != nil {
		return addressErr
	}

	stateStoreErr, shouldFallback := checkPortAvailableWithStateStore(ctx, protocol, address, port, log)
	if stateStoreErr == nil {
		return nil
	}
	if !shouldFallback {
		return stateStoreErr
	}
	if stateStoreErr != nil {
		log.V(1).Info("Warning: state store port availability check failed, falling back to MRU port availability check", "Error", stateStoreErr.Error())
	}
	return checkPortAvailableWithMruFile(ctx, protocol, address, port, log)
}

func ReserveSpecificPort(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) error {
	address, addressErr := normalizePortAllocationAddress(address)
	if addressErr != nil {
		return addressErr
	}

	stateStoreErr, shouldFallback := reserveSpecificPortWithStateStore(ctx, protocol, address, port, log)
	if stateStoreErr == nil {
		return nil
	}
	if !shouldFallback {
		return stateStoreErr
	}
	if stateStoreErr != nil {
		log.V(1).Info("Warning: state store port reservation failed, continuing without MRU reservation", "Error", stateStoreErr.Error())
	}
	return nil
}

func ReleaseSpecificPort(ctx context.Context, protocol apiv1.PortProtocol, address string, port int32, log logr.Logger) error {
	address, addressErr := normalizePortAllocationAddress(address)
	if addressErr != nil {
		return addressErr
	}

	stateStoreErr, shouldFallback := releaseSpecificPortWithStateStore(ctx, protocol, address, port, log)
	if stateStoreErr == nil {
		return nil
	}
	if !shouldFallback {
		return stateStoreErr
	}
	if stateStoreErr != nil {
		log.V(1).Info("Warning: state store port release failed, continuing without MRU release", "Error", stateStoreErr.Error())
	}
	return nil
}
