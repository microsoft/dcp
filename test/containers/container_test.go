/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"encoding/base64"
	"fmt"
	std_slices "slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	"github.com/microsoft/dcp/pkg/concurrency"
)

const (
	eventWatcherWarmupTimeout = 10 * time.Second
	eventCollectionTimeout    = 30 * time.Second
)

func TestContainerLifecycleMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		containerName := containertest.UniqueName(t, "container-lifecycle")
		require.NoError(t, tracker.TrackContainer(containerName))
		image := ensureTestImage(t, ctx, runtime)

		containerID, createErr := runtime.Orchestrator.CreateContainer(ctx, longRunningContainerOptions(
			containerName,
			image,
			tracker.Labels(),
		))
		require.NoError(t, createErr)
		require.NotEmpty(t, containerID)

		created := waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusCreated)
		require.Equal(t, containerName, created.Name)
		require.Equal(t, image, created.Image)
		require.Equal(t, tracker.RunID(), created.Labels[containertest.TestRunLabel])

		listed, listErr := runtime.Orchestrator.ListContainers(ctx, containers.ListContainersOptions{
			All: true,
			Filters: containers.ListContainersFilters{
				LabelFilters: []containers.LabelFilter{{
					Key:   containertest.TestRunLabel,
					Value: tracker.RunID(),
				}},
			},
		})
		require.NoError(t, listErr)
		require.NotEqual(t, -1, std_slices.IndexFunc(listed, func(container containers.ListedContainer) bool {
			return container.Id == containerID && container.Name == containerName
		}))

		started, startErr := runtime.Orchestrator.StartContainers(ctx, containers.StartContainersOptions{
			Containers: []string{containerName},
		})
		require.NoError(t, startErr)
		require.Equal(t, []string{containerName}, started)
		waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusRunning)

		stopped, stopErr := runtime.Orchestrator.StopContainers(ctx, containers.StopContainersOptions{
			Containers:    []string{containerName},
			SecondsToKill: 5,
		})
		require.NoError(t, stopErr)
		require.Equal(t, []string{containerName}, stopped)
		waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusExited)

		removed, removeErr := runtime.Orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerName},
		})
		require.NoError(t, removeErr)
		require.Equal(t, []string{containerName}, removed)
		waitForContainerAbsent(t, ctx, runtime.Orchestrator, containerName)
	})
}

func TestRunAndExecContainerMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		_, containerID := runLongLivedContainer(t, ctx, runtime, tracker, "run-exec")
		exitCode, stdout, stderr := execContainer(t, ctx, runtime.Orchestrator, containers.ExecContainerOptions{
			Container:        containerID,
			WorkingDirectory: "/tmp",
			Env: []containers.EnvVar{{
				Name:  "DCP_TEST_VALUE",
				Value: "exec-value",
			}},
			Command: containerProbePath,
			Args: []string{
				"report",
				"DCP_TEST_VALUE",
				"exec-stderr",
			},
		})

		require.Equal(t, int32(0), exitCode)
		require.Equal(t, "exec-value:/tmp", stdout)
		require.Equal(t, "exec-stderr", stderr)
	})
}

func TestCreateFilesMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		_, containerID := runLongLivedContainer(t, ctx, runtime, tracker, "create-files")
		createErr := runtime.Orchestrator.CreateFiles(ctx, containers.CreateFilesOptions{
			Container:   containerID,
			Destination: "/tmp",
			Entries: []containers.FileSystemEntry{{
				Type: containers.FileSystemEntryTypeDir,
				Name: "dcp-files",
				Entries: []containers.FileSystemEntry{
					{Name: "plain.txt", Contents: "plain-content"},
					{Name: "raw.txt", RawContents: base64.StdEncoding.EncodeToString([]byte("raw-content"))},
					{
						Type:   containers.FileSystemEntryTypeSymlink,
						Name:   "plain-link",
						Target: "plain.txt",
					},
				},
			}},
		})
		require.NoError(t, createErr)

		exitCode, stdout, stderr := execContainer(t, ctx, runtime.Orchestrator, containers.ExecContainerOptions{
			Container: containerID,
			Command:   containerProbePath,
			Args: []string{
				"inspect-files",
				"/tmp/dcp-files/plain.txt",
				"/tmp/dcp-files/raw.txt",
				"/tmp/dcp-files/plain-link",
			},
		})
		require.Equal(t, int32(0), exitCode)
		require.Equal(t, "plain-content|raw-content|plain.txt", stdout)
		require.Empty(t, stderr)
	})
}

func TestCaptureContainerLogsMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		stdout, stderr := runAndCapture(
			t,
			ctx,
			runtime,
			tracker,
			"container-logs",
			[]string{"emit", "stdout-marker\n", "stderr-marker\n"},
		)
		require.Equal(t, "stdout-marker\n", stdout)
		require.Equal(t, "stderr-marker\n", stderr)
	})
}

func TestAttachContainerMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		containerName := containertest.UniqueName(t, "attach-container")
		require.NoError(t, tracker.TrackContainer(containerName))

		containerID, createErr := runtime.Orchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
			Name:           containerName,
			Image:          ensureTestImage(t, ctx, runtime),
			Command:        []string{"interactive"},
			Labels:         tracker.Labels(),
			PullPolicy:     containers.PullPolicyNever,
			AttachTerminal: true,
		})
		require.NoError(t, createErr)

		_, startErr := runtime.Orchestrator.StartContainers(ctx, containers.StartContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, startErr)
		waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusRunning)

		terminalProcess, attachErr := runtime.Orchestrator.AttachContainer(ctx, containers.AttachContainerOptions{
			Container: containerID,
			Cols:      100,
			Rows:      30,
		})
		require.NoError(t, attachErr)
		require.NotNil(t, terminalProcess)
		require.NotNil(t, terminalProcess.PTY)
		require.NotNil(t, terminalProcess.ExitHandler)
		t.Cleanup(func() {
			_ = terminalProcess.PTY.Close()
			_ = terminalProcess.Stop()
		})
		terminalProcess.StartWaitForExit()
		require.NoError(t, terminalProcess.PTY.Resize(120, 40))

		_, writeErr := terminalProcess.PTY.Write([]byte("attach-success\n"))
		require.NoError(t, writeErr)
		output, readErr := readUntil(ctx, terminalProcess.PTY, "probe:attach-success")
		require.NoError(t, readErr, "terminal output: %q", output)

		_, writeErr = terminalProcess.PTY.Write([]byte("exit\n"))
		require.NoError(t, writeErr)
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for attach process exit: %v", ctx.Err())
		case <-terminalProcess.ExitHandler.Exited():
			exitInfo := terminalProcess.ExitHandler.ExitInfo()
			require.NoError(t, exitInfo.Err)
			require.Equal(t, int32(0), exitInfo.ExitCode)
		}
		waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusExited)
	})
}

func TestWatchContainersMethod(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		events := concurrency.NewUnboundedChan[containers.EventMessage](ctx)
		subscription, watchErr := runtime.Orchestrator.WatchContainers(events.In)
		require.NoError(t, watchErr)
		t.Cleanup(subscription.Cancel)

		warmContainerWatcher(t, ctx, runtime, tracker, events.Out)

		containerName := containertest.UniqueName(t, "watch-container")
		require.NoError(t, tracker.TrackContainer(containerName))
		containerID, createErr := runtime.Orchestrator.CreateContainer(ctx, longRunningContainerOptions(
			containerName,
			ensureTestImage(t, ctx, runtime),
			tracker.Labels(),
		))
		require.NoError(t, createErr)
		_, startErr := runtime.Orchestrator.StartContainers(ctx, containers.StartContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, startErr)
		waitForContainerStatus(t, ctx, runtime.Orchestrator, containerID, containers.ContainerStatusRunning)
		_, stopErr := runtime.Orchestrator.StopContainers(ctx, containers.StopContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, stopErr)
		_, removeErr := runtime.Orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
		})
		require.NoError(t, removeErr)

		collectionCtx, collectionCancel := context.WithTimeout(ctx, eventCollectionTimeout)
		actions, collectionErr := collectContainerActions(collectionCtx, events.Out, containerID)
		collectionCancel()
		require.Contains(t, actions, containers.EventActionCreate)
		require.Contains(t, actions, containers.EventActionStart)
		require.True(t,
			actions[containers.EventActionStop] ||
				actions[containers.EventActionDie] ||
				actions[containers.EventActionDied] ||
				actions[containers.EventActionDestroy],
			"expected a stop, die, died, or destroy event; got %v",
			actions,
		)
		require.NoError(t, collectionErr, "received container actions: %v", actions)

		subscription.Cancel()
		waitForEventChannelClosed(t, ctx, events.Out)
	})
}

func warmContainerWatcher(
	t *testing.T,
	ctx context.Context,
	runtime containertest.Runtime,
	tracker *containertest.ResourceTracker,
	events <-chan containers.EventMessage,
) {
	t.Helper()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		containerName := containertest.UniqueName(t, fmt.Sprintf("watch-warmup-%d", attempt))
		require.NoError(t, tracker.TrackContainer(containerName))
		containerID, runErr := runtime.Orchestrator.RunContainer(ctx, containers.RunContainerOptions{
			CreateContainerOptions: containers.CreateContainerOptions{
				Name:       containerName,
				Image:      ensureTestImage(t, ctx, runtime),
				Command:    []string{"exit"},
				Labels:     tracker.Labels(),
				PullPolicy: containers.PullPolicyNever,
			},
		})
		require.NoError(t, runErr)

		warmupCtx, warmupCancel := context.WithTimeout(ctx, eventWatcherWarmupTimeout)
		_, lastErr = waitForEvent(warmupCtx, events, func(event containers.EventMessage) bool {
			return event.Source == containers.EventSourceContainer && event.Actor.ID == containerID
		})
		warmupCancel()
		if lastErr == nil {
			return
		}
	}

	require.NoError(t, lastErr, "container event watcher did not become ready")
}

func collectContainerActions(
	ctx context.Context,
	events <-chan containers.EventMessage,
	containerID string,
) (map[containers.EventAction]bool, error) {
	actions := map[containers.EventAction]bool{}
	for {
		if actions[containers.EventActionCreate] &&
			actions[containers.EventActionStart] &&
			(actions[containers.EventActionStop] ||
				actions[containers.EventActionDie] ||
				actions[containers.EventActionDied] ||
				actions[containers.EventActionDestroy]) {
			return actions, nil
		}

		event, eventErr := waitForEvent(ctx, events, func(event containers.EventMessage) bool {
			return event.Source == containers.EventSourceContainer && event.Actor.ID == containerID
		})
		if eventErr != nil {
			return actions, eventErr
		}
		actions[event.Action] = true
	}
}

func TestCollectContainerActionsReturnsPartialResult(t *testing.T) {
	t.Parallel()

	events := make(chan containers.EventMessage, 1)
	events <- containers.EventMessage{
		Source: containers.EventSourceContainer,
		Action: containers.EventActionCreate,
		Actor:  containers.EventActor{ID: "container"},
	}
	close(events)

	actions, collectionErr := collectContainerActions(t.Context(), events, "container")

	require.Error(t, collectionErr)
	require.True(t, actions[containers.EventActionCreate])
	require.False(t, actions[containers.EventActionStart])
}

func waitForEvent(
	ctx context.Context,
	events <-chan containers.EventMessage,
	match func(containers.EventMessage) bool,
) (containers.EventMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return containers.EventMessage{}, ctx.Err()
		case event, open := <-events:
			if !open {
				return containers.EventMessage{}, fmt.Errorf("event channel closed")
			}
			if match(event) {
				return event, nil
			}
		}
	}
}

func waitForEventChannelClosed(t *testing.T, ctx context.Context, events <-chan containers.EventMessage) {
	t.Helper()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for event channel closure: %v", ctx.Err())
		case _, open := <-events:
			if !open {
				return
			}
		}
	}
}
