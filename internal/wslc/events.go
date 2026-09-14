/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"fmt"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/pubsub"
)

func (*WslcCliOrchestrator) WatchContainers(
	_ chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	return nil, fmt.Errorf("wslc container events are unsupported because the WSLC CLI does not expose a native event stream")
}

func (*WslcCliOrchestrator) WatchNetworks(
	_ chan<- containers.EventMessage,
) (*pubsub.Subscription[containers.EventMessage], error) {
	return nil, fmt.Errorf("wslc network events are unsupported because the WSLC CLI does not expose a native event stream")
}
