/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that PullImage returns the pulled reference without issuing an inspection command.
func TestPullImageReturnsReferenceWithoutInspection(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	executor := internal_testutil.NewTestProcessExecutor(ctx)
	t.Cleanup(func() { require.NoError(t, executor.Close()) })
	pullCommand := []string{"wslc", "image", "pull", "--quiet", "example.test/image:latest"}
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: pullCommand},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			_, writeErr := execution.Cmd.Stdout.Write([]byte("example.test/image:latest\n"))
			require.NoError(t, writeErr)
			return 0
		},
	})
	inspectCommand := []string{"wslc", "image", "inspect", "--format", "json", "example.test/image:latest"}
	orchestrator := NewWslcCliOrchestrator(testr.New(t), executor)
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "example.test/image:latest"})
	require.NoError(t, pullErr)
	require.Equal(t, "example.test/image:latest", imageID)
	require.Len(t, executor.FindAll(pullCommand, "", nil), 1)
	require.Empty(t, executor.FindAll(inspectCommand, "", nil))
}
