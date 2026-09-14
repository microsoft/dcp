/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeEventGateOnlySkipsWSLC(t *testing.T) {
	t.Parallel()

	for _, runtimeName := range []string{"docker", "podman", "wslc"} {
		var skipped bool
		t.Run(runtimeName, func(t *testing.T) {
			t.Cleanup(func() { skipped = t.Skipped() })
			SkipIfNativeRuntimeEventsUnavailable(t, Runtime{Name: runtimeName})
		})
		require.Equal(t, runtimeName == "wslc", skipped)
	}
}

func TestRuntimeRegistrationsIncludeSchedulingAndRecovery(t *testing.T) {
	t.Parallel()

	require.Contains(t, supportedRuntimeNames, "wslc")
	for _, runtimeName := range supportedRuntimeNames {
		require.NotNil(t, runtimeTestSlots[runtimeName], "runtime %q has no test slots", runtimeName)
		require.Positive(t, cap(runtimeTestSlots[runtimeName]))
		require.NotNil(t, runtimeRecovery[runtimeName], "runtime %q has no recovery state", runtimeName)
	}
}
