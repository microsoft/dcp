/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that WSLC is registered and every supported runtime has a positive test-slot limit and a recovery state.
func TestRuntimeRegistrationsIncludeSchedulingAndRecovery(t *testing.T) {
	t.Parallel()

	require.Contains(t, supportedRuntimeNames, "wslc")
	for _, runtimeName := range supportedRuntimeNames {
		require.NotNil(t, runtimeTestSlots[runtimeName], "runtime %q has no test slots", runtimeName)
		require.Positive(t, cap(runtimeTestSlots[runtimeName]))
		require.NotNil(t, runtimeRecovery[runtimeName], "runtime %q has no recovery state", runtimeName)
	}
}
