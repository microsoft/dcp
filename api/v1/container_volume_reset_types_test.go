/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that ContainerVolumeReset requires a target name and UID and rejects target changes.
func TestContainerVolumeResetValidation(t *testing.T) {
	ctx := context.Background()
	reset := &ContainerVolumeReset{Spec: ContainerVolumeResetSpec{ContainerName: "target", ContainerUID: "target-uid"}}
	require.Empty(t, reset.Validate(ctx))
	require.Empty(t, reset.ValidateUpdate(ctx, reset.DeepCopy()))
	changed := reset.DeepCopy()
	changed.Spec.ContainerName = "another"
	require.NotEmpty(t, changed.ValidateUpdate(ctx, reset))
	changed = reset.DeepCopy()
	changed.Spec.ContainerUID = "replacement"
	require.NotEmpty(t, changed.ValidateUpdate(ctx, reset))
	changed.Spec.ContainerUID = " "
	require.NotEmpty(t, changed.Validate(ctx))
	changed = reset.DeepCopy()
	changed.Spec.ContainerName = ""
	require.NotEmpty(t, changed.Validate(ctx))
}
