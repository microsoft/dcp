/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containertest

import (
	"testing"

	container_flags "github.com/microsoft/dcp/internal/containers/flags"
)

// SkipIfNativeRuntimeEventsUnavailable skips tests that require unavailable native event watches.
func SkipIfNativeRuntimeEventsUnavailable(t *testing.T, runtime Runtime) {
	t.Helper()

	if runtime.Name == string(container_flags.WslcRuntime) {
		t.Skip("WSLC CLI 2.9.11 does not expose native events; remove this gate when native container/network watches are implemented")
	}
}
