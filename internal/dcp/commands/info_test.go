/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerRuntimeInfoPreservesUnsupportedHostAddress(t *testing.T) {
	t.Parallel()

	info := containerRuntime{Runtime: "wslc", Installed: true, Running: true}
	encoded, marshalErr := json.Marshal(info)
	require.NoError(t, marshalErr)
	require.JSONEq(t, `{"runtime":"wslc","hostName":"","installed":true,"running":true}`, string(encoded))
}

func TestContainerRuntimeInfoPreservesSupportedHostAddress(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		runtime string
		host    string
	}{
		{runtime: "docker", host: "host.docker.internal"},
		{runtime: "podman", host: "host.containers.internal"},
	} {
		t.Run(testCase.runtime, func(t *testing.T) {
			t.Parallel()

			info := containerRuntime{
				Runtime: testCase.runtime, HostName: testCase.host,
				Installed: true, Running: true,
			}
			encoded, marshalErr := json.Marshal(info)
			require.NoError(t, marshalErr)
			var decoded containerRuntime
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			require.Equal(t, info, decoded)
		})
	}
}
