/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

// Verifies that WSLC inspection preserves separate entrypoint and command values, their
// combined arguments, and runtime-reported environment values.
func TestConvertInspectedContainerPreservesLaunchConfiguration(t *testing.T) {
	t.Parallel()
	var raw wslcInspectedContainer
	decodeErr := json.Unmarshal([]byte(`{"Id":"container-id","Name":"/container-name","Config":{"Entrypoint":["entrypoint","--entrypoint-arg"],"Cmd":["command","--command-arg"],"Env":["RUNTIME=value"]}}`), &raw)
	require.NoError(t, decodeErr)
	inspected, convertErr := convertInspectedContainer(raw, nil)
	require.NoError(t, convertErr)
	require.Equal(t, []string{"entrypoint", "--entrypoint-arg"}, inspected.Entrypoint)
	require.Equal(t, []string{"command", "--command-arg"}, inspected.Command)
	require.Equal(t, []string{"entrypoint", "--entrypoint-arg", "command", "--command-arg"}, inspected.Args)
	require.Equal(t, map[string]string{"RUNTIME": "value"}, inspected.Env)
}

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
