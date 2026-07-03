/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package applecontainer

import (
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/process"
)

func TestAppleContainerUsesSingleNetwork(t *testing.T) {
	log := testr.New(t)
	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	orchestrator := NewAppleContainerCliOrchestrator(log, executor)

	// The Apple runtime exposes only the built-in default network, so the controllers must take
	// the single-network path (skip custom-network create/connect/detach).
	require.True(t, containers.UsesSingleNetwork(orchestrator), "the Apple orchestrator must report UsesSingleNetwork() == true")
}
