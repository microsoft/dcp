/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that V1 and V2 container controllers observe a real process exiting after an exec-triggered condition, without a stop request.
// Both API versions must report the exit code and clean up their managed containers.
func TestContainerRuntimeExitEventsWithRealOrchestrator(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(testCancel)
	containertest.ForEachHealthyRuntime(t, testCtx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		ctx, cancel := context.WithCancel(runtimeCtx)
		defer cancel()
		tracker := containertest.NewResourceTracker(t, runtime)
		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{Image: "busybox:latest"})
		require.NoError(t, pullErr)

		serverInfo, environmentInfo, startupErr := StartAdvancedTestEnvironmentWithOptions(
			ctx,
			ContainerController|PhysicalContainerController|PhysicalContainerImageController,
			containertest.UniqueName(t, "runtime-exit-events"),
			t.TempDir(),
			AdvancedTestEnvironmentOptions{
				ApiServerFlags:        ctrl_testutil.ApiServerUseTrueContainerOrchestrator,
				ContainerOrchestrator: runtime.Orchestrator,
			},
		)
		require.NoError(t, startupErr)
		defer environmentInfo.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, ctx, cancel, serverInfo)

		const exitFile = "/tmp/dcp-exit"
		const releaseFile = "/tmp/dcp-release"
		const waitCommand = "while [ ! -f " + exitFile + " ]; do sleep 0.1; done; " +
			"while [ ! -f " + releaseFile + " ]; do sleep 0.1; done"
		for _, apiVersion := range []string{"v1", "v2"} {
			t.Run(apiVersion, func(t *testing.T) {
				containerName := containertest.UniqueName(t, "runtime-exit-"+apiVersion)
				require.NoError(t, tracker.TrackContainer(containerName))
				var containerID string
				var observeExit func()

				if apiVersion == "v1" {
					resource := &apiv1.Container{
						ObjectMeta: metav1.ObjectMeta{Name: containerName},
						Spec: apiv1.ContainerSpec{
							ContainerName: containerName,
							Image:         imageID,
							Command:       "sh",
							Args:          []string{"-c", waitCommand},
						},
					}
					for _, label := range tracker.Labels() {
						resource.Spec.Labels = append(resource.Spec.Labels, apiv1.ContainerLabel{Key: label.Key, Value: label.Value})
					}
					require.NoError(t, serverInfo.Client.Create(ctx, resource))
					running := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, resource.NamespacedName(), func(current *apiv1.Container) (bool, error) {
						if current.Status.State == apiv1.ContainerStateFailedToStart {
							return false, fmt.Errorf("container failed to start: %s", current.Status.Message)
						}
						return current.Status.State == apiv1.ContainerStateRunning, nil
					})
					containerID = running.Status.ContainerID
					observeExit = func() {
						exited := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, resource.NamespacedName(), func(current *apiv1.Container) (bool, error) {
							return current.Status.State == apiv1.ContainerStateExited && current.Status.ExitCode != nil, nil
						})
						require.Zero(t, *exited.Status.ExitCode)
						require.NoError(t, serverInfo.Client.Delete(ctx, resource))
						ctrl_testutil.WaitObjectDeleted[apiv1.Container](t, ctx, serverInfo.Client, resource)
					}
				} else {
					namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{
						Name: containertest.UniqueName(t, "runtime-exit-namespace"),
					}}
					require.NoError(t, serverInfo.Client.Create(ctx, namespace))
					waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: namespace.Name}, func(current *apiv2.Namespace) (bool, error) {
						return current.Status.Phase == apiv2.NamespacePhaseActive, nil
					})
					image := &apiv2.PhysicalContainerImage{
						ObjectMeta: metav1.ObjectMeta{Name: "image", Namespace: namespace.Name},
						Spec:       apiv2.PhysicalContainerImageSpec{ImageID: imageID},
					}
					require.NoError(t, serverInfo.Client.Create(ctx, image))
					waitObjectAssumesStateEx(t, ctx, serverInfo.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
						return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
					})
					resource := &apiv2.PhysicalContainer{
						ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
						Spec: apiv2.PhysicalContainerSpec{Container: &apiv2.PhysicalContainerConfig{
							ImageRef:      image.Name,
							ContainerName: containerName,
							Entrypoint:    "sh",
							Command:       []string{"-c", waitCommand},
							Labels:        tracker.Labels(),
						}},
					}
					require.NoError(t, serverInfo.Client.Create(ctx, resource))
					running := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, resource.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
						if current.Status.Phase == apiv2.PhysicalContainerPhaseFailed {
							return false, fmt.Errorf("physical container failed: %v", current.Status.Conditions)
						}
						return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
					})
					containerID = running.Status.ContainerID
					observeExit = func() {
						exited := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, resource.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
							return current.Status.Phase == apiv2.PhysicalContainerPhaseExited && current.Status.ExitCode != nil, nil
						})
						require.Zero(t, *exited.Status.ExitCode)
						require.Equal(t, string(containers.ContainerStatusExited), exited.Status.RuntimeStatus)
						require.NoError(t, serverInfo.Client.Delete(ctx, resource))
						ctrl_testutil.WaitObjectDeleted[apiv2.PhysicalContainer](t, ctx, serverInfo.Client, resource)
					}
				}

				require.NotEmpty(t, containerID)
				exitCodes, execErr := runtime.Orchestrator.ExecContainer(ctx, containers.ExecContainerOptions{
					Container: containerID,
					Command:   "touch",
					Args:      []string{exitFile},
				})
				require.NoError(t, execErr)
				select {
				case exitCode, open := <-exitCodes:
					require.True(t, open, "exec closed without an exit code")
					require.Zero(t, exitCode)
				case <-ctx.Done():
					t.Fatalf("waiting for exit trigger: %v", ctx.Err())
				}
				releaseErr := runtime.Orchestrator.CreateFiles(ctx, containers.CreateFilesOptions{
					Container:   containerID,
					Destination: "/tmp",
					Entries:     []containers.FileSystemEntry{{Name: "dcp-release"}},
				})
				require.NoError(t, releaseErr)
				observeExit()
			})
		}
	})
}
