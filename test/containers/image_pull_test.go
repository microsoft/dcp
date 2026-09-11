/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
)

const pullImageReference = "mcr.microsoft.com/azurelinux/distroless/minimal:3.0.20260809"

func TestPullAndInspectImageMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{
			Image: pullImageReference,
		})
		require.NoError(t, pullErr)
		require.NotEmpty(t, imageID)

		byName, inspectNameErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{pullImageReference},
		})
		require.NoError(t, inspectNameErr)
		require.Len(t, byName, 1)
		require.NotEmpty(t, byName[0].Id)
		require.Contains(t, byName[0].Tags, pullImageReference)

		byID, inspectIDErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{imageID},
		})
		require.NoError(t, inspectIDErr)
		require.Len(t, byID, 1)
		require.NotEmpty(t, byID[0].Id)
		require.Equal(t, byName[0].Id, byID[0].Id)
		require.Contains(t, byID[0].Tags, pullImageReference)
	})
}
