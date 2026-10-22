/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that ContainerVolume generations accept nonnegative int64 values and can increase,
// but cannot decrease or change the logical volume name.
func TestContainerVolumeGenerationValidation(t *testing.T) {
	ctx := context.Background()
	volume := &ContainerVolume{Spec: ContainerVolumeSpec{Name: "data"}}
	require.Empty(t, volume.Validate(ctx))
	require.Empty(t, volume.ValidateUpdate(ctx, volume.DeepCopy()))
	for _, generation := range []int64{1, math.MaxInt64} {
		increased := volume.DeepCopy()
		increased.Spec.Generation = generation
		require.Empty(t, increased.Validate(ctx))
		require.Empty(t, increased.ValidateUpdate(ctx, volume))
		require.NotEmpty(t, volume.ValidateUpdate(ctx, increased))
	}
	negative := volume.DeepCopy()
	negative.Spec.Generation = -1
	require.NotEmpty(t, negative.Validate(ctx))
	require.NotEmpty(t, negative.ValidateUpdate(ctx, volume))
	renamed := volume.DeepCopy()
	renamed.Spec.Name = "other"
	require.NotEmpty(t, renamed.ValidateUpdate(ctx, volume))
}
