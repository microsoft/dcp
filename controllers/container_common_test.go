/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/testutil"
)

type layerBaseImageOrchestrator struct {
	containers.ContainerOrchestrator
	pulls               int
	postPullInspections int
	postPullError       error
}

func (orchestrator *layerBaseImageOrchestrator) PullImage(_ context.Context, options containers.PullImageOptions) (string, error) {
	orchestrator.pulls++
	return options.Image, nil
}

func (orchestrator *layerBaseImageOrchestrator) InspectImages(_ context.Context, _ containers.InspectImagesOptions) ([]containers.InspectedImage, error) {
	if orchestrator.pulls == 0 {
		return nil, containers.ErrNotFound
	}
	orchestrator.postPullInspections++
	if orchestrator.postPullInspections == 1 {
		return nil, orchestrator.postPullError
	}
	return []containers.InspectedImage{{Id: "sha256:resolved-base-image"}}, nil
}

// Verifies that ensureBaseImageForLayers retries transient post-pull inspection failures
// for always and missing policies without pulling the base image again.
func TestEnsureBaseImageForLayersRetriesInspectionAfterPull(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		policy containers.ImagePullPolicy
		err    error
	}{
		{name: "always", policy: containers.PullPolicyAlways, err: errors.New("image metadata temporarily unavailable")},
		{name: "missing", policy: containers.PullPolicyMissing, err: containers.ErrNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			orchestrator := &layerBaseImageOrchestrator{postPullError: testCase.err}
			inspected, inspectErr := ensureBaseImageForLayers(ctx, orchestrator, "example:latest", testCase.policy, logr.Discard())
			require.NoError(t, inspectErr)
			require.Equal(t, "sha256:resolved-base-image", inspected.Id)
			require.Equal(t, 1, orchestrator.pulls)
			require.Equal(t, 2, orchestrator.postPullInspections)
		})
	}
}
