/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestPhysicalContainerPortMappingsFromInspected(t *testing.T) {
	t.Parallel()

	portMappings, mappingErr := physicalContainerPortMappingsFromInspected(containers.InspectedContainerPortMapping{
		"7070": []containers.InspectedContainerHostPortConfig{
			{HostPort: "17070"},
		},
		"8080/tcp": []containers.InspectedContainerHostPortConfig{
			{HostIp: "::1", HostPort: "18080"},
			{HostIp: "127.0.0.1", HostPort: "18080"},
		},
		"9090/udp": nil,
	})

	require.NoError(t, mappingErr)
	require.Equal(t, []apiv2.PhysicalContainerPortMapping{
		{
			ContainerPort: 7070,
			Protocol:      commonapi.PortProtocolTCP,
			HostPort:      17070,
		},
		{
			ContainerPort: 8080,
			Protocol:      commonapi.PortProtocolTCP,
			HostIP:        "127.0.0.1",
			HostPort:      18080,
		},
		{
			ContainerPort: 8080,
			Protocol:      commonapi.PortProtocolTCP,
			HostIP:        "::1",
			HostPort:      18080,
		},
		{
			ContainerPort: 9090,
			Protocol:      commonapi.PortProtocolUDP,
		},
	}, portMappings)
}

func TestApplyInspectedPhysicalContainerStatusRequestsReconciliationOnlyWhenUseful(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                     string
		runtimeStatus            containers.ContainerStatus
		expectedPhase            apiv2.PhysicalContainerPhase
		expectReconciliationNeed bool
	}{
		{
			// Running containers rely on runtime events, but keep a slow poll so a missed event cannot strand them.
			name:                     "running keeps polling",
			runtimeStatus:            containers.ContainerStatusRunning,
			expectedPhase:            apiv2.PhysicalContainerPhaseRunning,
			expectReconciliationNeed: true,
		},
		{
			name:                     "exited goes quiet",
			runtimeStatus:            containers.ContainerStatusExited,
			expectedPhase:            apiv2.PhysicalContainerPhaseExited,
			expectReconciliationNeed: false,
		},
		{
			name:                     "dead goes quiet",
			runtimeStatus:            containers.ContainerStatusDead,
			expectedPhase:            apiv2.PhysicalContainerPhaseExited,
			expectReconciliationNeed: false,
		},
		{
			name:                     "created keeps polling",
			runtimeStatus:            containers.ContainerStatusCreated,
			expectedPhase:            apiv2.PhysicalContainerPhasePending,
			expectReconciliationNeed: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			container := &apiv2.PhysicalContainer{}
			inspected := &containers.InspectedContainer{
				Id:     "container-id",
				Name:   "container-name",
				Status: tc.runtimeStatus,
			}

			change := applyInspectedPhysicalContainerStatus(container, inspected, logr.Discard())

			require.Equal(t, tc.expectedPhase, container.Status.Phase)
			require.Equal(t, tc.expectReconciliationNeed, (change&additionalReconciliationNeeded) != 0)
		})
	}
}
