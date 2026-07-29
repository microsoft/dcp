/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

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
			Protocol:      commonapi.TCP,
			HostPort:      17070,
		},
		{
			ContainerPort: 8080,
			Protocol:      commonapi.TCP,
			HostIP:        "127.0.0.1",
			HostPort:      18080,
		},
		{
			ContainerPort: 8080,
			Protocol:      commonapi.TCP,
			HostIP:        "::1",
			HostPort:      18080,
		},
		{
			ContainerPort: 9090,
			Protocol:      commonapi.UDP,
		},
	}, portMappings)
}
