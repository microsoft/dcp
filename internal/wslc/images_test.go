/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that PullImage retries missing post-pull identity inspection without repeating
// the successful WSLC pull command.
func TestPullImageRetriesIdentityInspection(t *testing.T) {
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
	var inspectionAttempts atomic.Int32
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: inspectCommand},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			if inspectionAttempts.Add(1) <= 2 {
				_, stderrErr := execution.Cmd.Stderr.Write([]byte("Image 'example.test/image:latest' not found.\n"))
				require.NoError(t, stderrErr)
				return 1
			}
			_, writeErr := execution.Cmd.Stdout.Write([]byte("[{\"Id\":\"sha256:image-id\"}]\n"))
			require.NoError(t, writeErr)
			return 0
		},
	})
	orchestrator := NewWslcCliOrchestrator(testr.New(t), executor)
	imageID, pullErr := orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "example.test/image:latest"})
	require.NoError(t, pullErr)
	require.Equal(t, "sha256:image-id", imageID)
	require.Len(t, executor.FindAll(pullCommand, "", nil), 1)
	require.Len(t, executor.FindAll(inspectCommand, "", nil), 3)
}
