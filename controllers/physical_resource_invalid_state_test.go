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
)

// Verifies that an unsupported physical container state becomes terminal and clears stale cleanup diagnostics.
func TestUnknownPhysicalContainerStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerData{
		state:          physicalContainerState(999),
		cleanupMessage: "old cleanup failure",
	}

	change := handleUnknownPhysicalContainerDataReason(
		t.Context(),
		nil,
		&apiv2.PhysicalContainer{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerStateInvalid, data.state)
	require.False(t, data.workInProgress)
	require.Equal(t, "Physical container reached invalid reconciliation state physicalContainerState(999).", data.failureMessage)
	require.Empty(t, data.cleanupMessage)
}

// Verifies that the PhysicalContainerImage invalid-state handler records a terminal failure
// for an unsupported reconciliation state.
func TestUnknownPhysicalContainerImageStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerImageData{
		state: physicalContainerImageState(999),
	}

	change := handleUnknownPhysicalContainerImageState(
		t.Context(),
		nil,
		&apiv2.PhysicalContainerImage{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerImageStateInvalid, data.state)
	require.False(t, data.workInProgress)
	require.Equal(t, "PhysicalContainerImage reached invalid reconciliation state physicalContainerImageState(999).", data.failureMessage)
}

// Verifies that an unsupported physical network state becomes terminal.
func TestUnknownPhysicalContainerNetworkStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerNetworkData{
		state: physicalContainerNetworkState(999),
	}

	change := handleUnknownPhysicalContainerNetworkDataReason(
		t.Context(),
		nil,
		&apiv2.PhysicalContainerNetwork{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerNetworkStateInvalid, data.state)
	require.False(t, data.workInProgress)
	require.Equal(t, "Physical container network reached invalid reconciliation state physicalContainerNetworkState(999).", data.failureMessage)
}

// Verifies that an unsupported physical volume state becomes terminal.
func TestUnknownPhysicalContainerVolumeStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerVolumeData{
		state: physicalContainerVolumeState(999),
	}

	change := handleUnknownPhysicalContainerVolumeDataReason(
		t.Context(),
		nil,
		&apiv2.PhysicalContainerVolume{},
		data.state,
		data,
		logr.Discard(),
	)

	require.Equal(t, noChange, change)
	require.Equal(t, physicalContainerVolumeStateInvalid, data.state)
	require.False(t, data.workInProgress)
	require.Equal(t, "Physical container volume reached invalid reconciliation state physicalContainerVolumeState(999).", data.failureMessage)
}
