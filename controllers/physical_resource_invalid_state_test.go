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

func TestUnknownPhysicalContainerStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerData{
		state:          physicalContainerStateStop,
		progress:       physicalResourceProgressCompleted,
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
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "Physical container reached invalid reconciliation state Stop with progress Completed.", data.failureMessage)
	require.Empty(t, data.cleanupMessage)
}

// Verifies that the PhysicalContainerImage invalid-state handler records a terminal failure
// for an unsupported reconciliation state/progress pair.
func TestUnknownPhysicalContainerImageStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerImageData{
		state:    physicalContainerImageStateBuild,
		progress: physicalResourceProgressSkipped,
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
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "PhysicalContainerImage reached invalid reconciliation state Build with progress Skipped.", data.failureMessage)
}

func TestUnknownPhysicalContainerNetworkStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerNetworkData{
		state:    physicalContainerNetworkStateRuntime,
		progress: physicalResourceProgressInProgress,
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
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "Physical container network reached invalid reconciliation state Runtime with progress InProgress.", data.failureMessage)
}

func TestUnknownPhysicalContainerVolumeStateBecomesTerminal(t *testing.T) {
	t.Parallel()

	data := &physicalContainerVolumeData{
		state:    physicalContainerVolumeStateRemove,
		progress: physicalResourceProgressCompleted,
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
	require.Equal(t, physicalResourceProgressFailed, data.progress)
	require.Equal(t, "Physical container volume reached invalid reconciliation state Remove with progress Completed.", data.failureMessage)
}
