/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package networking

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Gets a free TCP or UDP port for a given address (defaults to localhost).
// Even if this method is called twice in a row, it should not return the same port.
func GetFreePort(ctx context.Context, protocol commonapi.PortProtocol, address string, log logr.Logger) (int32, error) {
	protocol = NormalizePortProtocol(protocol)
	address, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return 0, addressErr
	}

	stateStorePort, _, shouldFallback, stateStoreErr := allocatePortFromStateStoreRange(ctx, protocol, address, log)
	if stateStoreErr == nil {
		return stateStorePort, nil
	}
	if !shouldFallback {
		return 0, stateStoreErr
	}
	log.V(1).Info("Warning: state store port allocation failed, falling back to MRU port allocation", "Error", stateStoreErr.Error())
	return allocateRandomEphemeralPortWithMruFile(ctx, protocol, address, log)
}

func CheckPortAvailable(ctx context.Context, protocol commonapi.PortProtocol, address string, port int32, log logr.Logger) error {
	protocol = NormalizePortProtocol(protocol)
	address, addressErr := NormalizePortAllocationAddress(address)
	if addressErr != nil {
		return addressErr
	}

	_, shouldFallback, stateStoreErr := checkPortAvailableWithStateStore(ctx, protocol, address, port, log)
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
