/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/commandstreams"
	"github.com/microsoft/dcp/pkg/process"
)

// Verifies that WSLc exec forwards caller-owned stdin without a TTY, preserves raw duplex IO,
// keeps stderr separate and retains its existing completion, cancellation and startup-failure behavior.
func TestExecContainerStreams(t *testing.T) {
	t.Parallel()
	commandstreams.VerifyCommandStreams(t, []string{"wslc", "container", "exec"},
		func(ctx context.Context, executor process.Executor, stdin io.Reader, stdout, stderr io.WriteCloser) (<-chan int32, error) {
			orchestrator := NewWslcCliOrchestrator(testr.New(t), executor)
			return orchestrator.ExecContainer(ctx, containers.ExecContainerOptions{
				Container: "pinned-id", Command: "adapter", Args: []string{"--flag"}, StdInStream: stdin,
				StreamCommandOptions: containers.StreamCommandOptions{StdOutStream: stdout, StdErrStream: stderr},
			})
		})
}

// Verifies that applyCreateContainerOptions rejects options unsupported by WSLC.
func TestApplyCreateContainerOptionsRejectsUnsupportedSettings(t *testing.T) {
	t.Parallel()

	_, restartErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image:         "image",
		RestartPolicy: containers.RestartPolicyAlways,
	})
	require.ErrorContains(t, restartErr, "restart policy")

	_, startIntervalErr := applyCreateContainerOptions(nil, containers.CreateContainerOptions{
		Image: "image",
		Healthcheck: containers.ContainerHealthcheck{
			StartInterval: time.Second,
		},
	})
	require.ErrorContains(t, startIntervalErr, "start intervals")
}
