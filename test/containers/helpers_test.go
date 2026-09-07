/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

const pollImmediately = true

func forEachHealthyRuntime(
	t *testing.T,
	run func(t *testing.T, ctx context.Context, runtime containertest.Runtime),
) {
	t.Helper()

	testCtx, testCancel := testutil.GetTestContext(t, testOperationTimeout)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, run)
}

func longRunningContainerOptions(
	name string,
	image string,
	labels []containers.Label,
) containers.CreateContainerOptions {
	return containers.CreateContainerOptions{
		Name:       name,
		Image:      image,
		Command:    []string{"sh", "-c", "sleep 120"},
		Labels:     labels,
		PullPolicy: containers.PullPolicyNever,
	}
}

func runLongLivedContainer(
	t *testing.T,
	ctx context.Context,
	runtime containertest.Runtime,
	tracker *containertest.ResourceTracker,
	prefix string,
) (name string, id string) {
	t.Helper()

	image := ensureBaseImage(t, ctx, runtime)
	name = containertest.UniqueName(t, prefix)
	require.NoError(t, tracker.TrackContainer(name))

	id, runErr := runtime.Orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: longRunningContainerOptions(name, image, tracker.Labels()),
	})
	require.NoError(t, runErr)
	require.NotEmpty(t, id)
	waitForContainerStatus(t, ctx, runtime.Orchestrator, id, containers.ContainerStatusRunning)
	return name, id
}

func waitForContainerStatus(
	t *testing.T,
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	container string,
	status containers.ContainerStatus,
) containers.InspectedContainer {
	t.Helper()

	var result containers.InspectedContainer
	waitErr := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, pollImmediately, func(ctx context.Context) (bool, error) {
		inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{container},
		})
		if inspectErr != nil {
			return false, inspectErr
		}
		if len(inspected) != 1 {
			return false, nil
		}
		result = inspected[0]
		return result.Status == status, nil
	})
	require.NoError(t, waitErr)
	return result
}

func waitForContainerAbsent(t *testing.T, ctx context.Context, orchestrator containers.ContainerOrchestrator, container string) {
	t.Helper()
	waitForObjectAbsent(t, ctx, func(ctx context.Context) (int, error) {
		inspected, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
			Containers: []string{container},
		})
		return len(inspected), inspectErr
	})
}

func waitForNetworkAbsent(t *testing.T, ctx context.Context, orchestrator containers.ContainerOrchestrator, network string) {
	t.Helper()
	waitForObjectAbsent(t, ctx, func(ctx context.Context) (int, error) {
		inspected, inspectErr := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
			Networks: []string{network},
		})
		return len(inspected), inspectErr
	})
}

func waitForVolumeAbsent(t *testing.T, ctx context.Context, orchestrator containers.ContainerOrchestrator, volume string) {
	t.Helper()
	waitForObjectAbsent(t, ctx, func(ctx context.Context) (int, error) {
		inspected, inspectErr := orchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{
			Volumes: []string{volume},
		})
		return len(inspected), inspectErr
	})
}

func waitForImageAbsent(t *testing.T, ctx context.Context, orchestrator containers.ContainerOrchestrator, image string) {
	t.Helper()
	waitForObjectAbsent(t, ctx, func(ctx context.Context) (int, error) {
		inspected, inspectErr := orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{image},
		})
		return len(inspected), inspectErr
	})
}

func waitForObjectAbsent(
	t *testing.T,
	ctx context.Context,
	inspect func(context.Context) (count int, err error),
) {
	t.Helper()

	waitErr := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, pollImmediately, func(ctx context.Context) (bool, error) {
		count, inspectErr := inspect(ctx)
		if count > 0 {
			return false, nil
		}
		if inspectErr == nil || errors.Is(inspectErr, containers.ErrNotFound) {
			return true, nil
		}
		return false, inspectErr
	})
	require.NoError(t, waitErr)
}

func execContainer(
	t *testing.T,
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	options containers.ExecContainerOptions,
) (int32, string, string) {
	t.Helper()

	stdout := testutil.NewBufferWriter()
	stderr := testutil.NewBufferWriter()
	t.Cleanup(func() {
		_ = stdout.Close()
		_ = stderr.Close()
	})
	options.StdOutStream = stdout
	options.StdErrStream = stderr

	exitCodes, execErr := orchestrator.ExecContainer(ctx, options)
	require.NoError(t, execErr)

	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for container exec: %v", ctx.Err())
	case exitCode, open := <-exitCodes:
		require.True(t, open, "container exec exit channel closed without an exit code")
		return exitCode, string(stdout.Bytes()), string(stderr.Bytes())
	}
	return 0, "", ""
}

func captureContainerLogs(
	t *testing.T,
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	container string,
) (string, string) {
	t.Helper()

	stdout := testutil.NewBufferWriter()
	stderr := testutil.NewBufferWriter()
	captureErr := orchestrator.CaptureContainerLogs(
		ctx,
		container,
		stdout,
		stderr,
		containers.StreamContainerLogsOptions{},
	)
	require.NoError(t, captureErr)

	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for container stdout logs: %v", ctx.Err())
	case <-stdout.Closed():
	}
	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for container stderr logs: %v", ctx.Err())
	case <-stderr.Closed():
	}

	return string(stdout.Bytes()), string(stderr.Bytes())
}

func runAndCapture(
	t *testing.T,
	ctx context.Context,
	runtime containertest.Runtime,
	tracker *containertest.ResourceTracker,
	prefix string,
	command []string,
) (string, string) {
	t.Helper()

	return runImageAndCapture(
		t,
		ctx,
		runtime,
		tracker,
		prefix,
		ensureBaseImage(t, ctx, runtime),
		command,
	)
}

func runImageAndCapture(
	t *testing.T,
	ctx context.Context,
	runtime containertest.Runtime,
	tracker *containertest.ResourceTracker,
	prefix string,
	image string,
	command []string,
) (string, string) {
	t.Helper()

	name := containertest.UniqueName(t, prefix)
	require.NoError(t, tracker.TrackContainer(name))

	containerID, runErr := runtime.Orchestrator.RunContainer(ctx, containers.RunContainerOptions{
		CreateContainerOptions: containers.CreateContainerOptions{
			Name:       name,
			Image:      image,
			Command:    command,
			Labels:     tracker.Labels(),
			PullPolicy: containers.PullPolicyNever,
		},
	})
	require.NoError(t, runErr)
	require.NotEmpty(t, containerID)
	waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusExited)
	stdout, stderr := captureContainerLogs(t, ctx, runtime.Orchestrator, containerID)

	_, removeErr := runtime.Orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{containerID},
		Force:      true,
	})
	require.NoError(t, removeErr)
	waitForContainerAbsent(t, ctx, runtime.Orchestrator, containerID)

	return stdout, stderr
}

func imageReference(t *testing.T, prefix string) string {
	t.Helper()
	return "localhost/" + containertest.UniqueName(t, prefix) + ":latest"
}

func writeBuildContext(t *testing.T, baseImage string, marker string) (contextDir string, dockerfilePath string) {
	t.Helper()

	contextDir = t.TempDir()
	dockerfilePath = filepath.Join(contextDir, "Dockerfile")
	dockerfile := fmt.Sprintf("FROM %s\nRUN printf '%%s' '%s' > /dcp-build-marker\n", baseImage, marker)
	require.NoError(t, usvc_io.WriteFile(dockerfilePath, []byte(dockerfile), osutil.PermissionOnlyOwnerReadWrite))
	return contextDir, dockerfilePath
}

func rawImageLayer(t *testing.T, path string, content string) string {
	t.Helper()

	var layer bytes.Buffer
	tarWriter := tar.NewWriter(&layer)
	contentBytes := []byte(content)
	require.NoError(t, tarWriter.WriteHeader(&tar.Header{
		Name: path,
		Mode: 0644,
		Size: int64(len(contentBytes)),
	}))
	_, writeErr := tarWriter.Write(contentBytes)
	require.NoError(t, writeErr)
	require.NoError(t, tarWriter.Close())
	return base64.StdEncoding.EncodeToString(layer.Bytes())
}

func readUntil(ctx context.Context, reader io.Reader, target string) (string, error) {
	var accumulated strings.Builder

	for {
		if strings.Contains(accumulated.String(), target) {
			return accumulated.String(), nil
		}

		buffer := make([]byte, 4096)
		type readResult struct {
			count int
			err   error
		}
		resultChannel := make(chan readResult, 1)
		go func() {
			count, readErr := reader.Read(buffer)
			resultChannel <- readResult{count: count, err: readErr}
		}()

		select {
		case <-ctx.Done():
			return accumulated.String(), ctx.Err()
		case result := <-resultChannel:
			if result.count > 0 {
				accumulated.Write(buffer[:result.count])
			}
			if result.err != nil {
				if strings.Contains(accumulated.String(), target) {
					return accumulated.String(), nil
				}
				return accumulated.String(), result.err
			}
		}
	}
}
