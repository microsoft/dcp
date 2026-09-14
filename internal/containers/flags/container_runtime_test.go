/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package flags

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestRuntimeFlagValues(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		input string
		want  RuntimeFlagValue
	}{
		{input: "", want: UnknownRuntime},
		{input: "docker", want: DockerRuntime},
		{input: "PODMAN", want: PodmanRuntime},
		{input: "wslc", want: WslcRuntime},
		{input: "WsLc", want: WslcRuntime},
	} {
		t.Run(testCase.input, func(t *testing.T) {
			t.Parallel()

			var value RuntimeFlagValue
			require.NoError(t, value.Set(testCase.input))
			require.Equal(t, testCase.want, value)
		})
	}
}

func TestRuntimeFlagRejectsUnknownWithoutChangingValue(t *testing.T) {
	t.Parallel()

	value := WslcRuntime
	require.ErrorContains(t, value.Set("unknown"), "docker, podman, wslc")
	require.Equal(t, WslcRuntime, value)
}

func TestRuntimeFlagHelpIncludesWSLC(t *testing.T) {
	flagSet := pflag.NewFlagSet(t.Name(), pflag.ContinueOnError)
	EnsureRuntimeFlag(flagSet)

	runtimeFlag := flagSet.Lookup(RuntimeFlagName)
	require.NotNil(t, runtimeFlag)
	require.Contains(t, runtimeFlag.Usage, "docker, podman, wslc")
}
