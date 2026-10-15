/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

// Verifies that applyCreateContainerOptions rejects options unsupported by WSLC.
func TestApplyCreateContainerOptionsRejectsUnsupportedSettings(t *testing.T) {
	t.Parallel()

	_, restartErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image:         "image",
		RestartPolicy: containers.RestartPolicyAlways,
	})
	require.ErrorContains(t, restartErr, "restart policy")

	_, startIntervalErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image: "image",
		Healthcheck: containers.ContainerHealthcheck{
			StartInterval: time.Second,
		},
	})
	require.ErrorContains(t, startIntervalErr, "start intervals")
}
