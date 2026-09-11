/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	localRegistryContainerPort = 5000
	localRegistryPollInterval  = 200 * time.Millisecond
)

func TestPullAndInspectImageMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		tracker := containertest.NewResourceTracker(t, runtime)

		registryBinaryPath, registryBinaryErr := internal_testutil.GetTestToolPath("oci_registry_c")
		require.NoError(t, registryBinaryErr)

		registryImage := imageReference(t, "local-registry")
		require.NoError(t, tracker.TrackImage(registryImage))
		buildRegistryImage(t, ctx, runtime.Orchestrator, tracker, registryBinaryPath, registryImage)

		repository := containertest.UniqueName(t, "dcp-local-image")
		imageLabels, labelsErr := json.Marshal(tracker.MapLabels())
		require.NoError(t, labelsErr)

		registryContainer := containertest.UniqueName(t, "local-registry")
		require.NoError(t, tracker.TrackContainer(registryContainer))
		registryContainerID, runErr := runtime.Orchestrator.RunContainer(ctx, containers.RunContainerOptions{
			CreateContainerOptions: containers.CreateContainerOptions{
				Name:       registryContainer,
				Image:      registryImage,
				Labels:     tracker.Labels(),
				PullPolicy: containers.PullPolicyNever,
				Ports: []containers.CreateContainerPort{{
					ContainerPort: localRegistryContainerPort,
				}},
				Env: []containers.EnvVar{
					{Name: "DCP_REGISTRY_REPOSITORY", Value: repository},
					{Name: "DCP_IMAGE_LABELS", Value: string(imageLabels)},
				},
			},
		})
		require.NoError(t, runErr)
		require.NotEmpty(t, registryContainerID)

		inspectedContainer := waitForContainerStatus(
			t,
			ctx,
			runtime.Orchestrator,
			registryContainerID,
			containers.ContainerStatusRunning,
		)
		portBindings := inspectedContainer.Ports[fmt.Sprintf("%d/tcp", localRegistryContainerPort)]
		require.Len(t, portBindings, 1)
		hostPort, portErr := strconv.Atoi(portBindings[0].HostPort)
		require.NoError(t, portErr)
		require.Positive(t, hostPort)

		registryURL := fmt.Sprintf("http://127.0.0.1:%d/v2/", hostPort)
		waitErr := wait.PollUntilContextCancel(ctx, localRegistryPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, registryURL, nil)
			if requestErr != nil {
				return false, requestErr
			}
			response, getErr := http.DefaultClient.Do(request)
			if getErr != nil {
				return false, nil
			}
			defer response.Body.Close()
			return response.StatusCode == http.StatusOK, nil
		})
		require.NoError(t, waitErr)

		localImageReference := fmt.Sprintf("127.0.0.1:%d/%s:latest", hostPort, repository)
		require.NoError(t, tracker.TrackImage(localImageReference))
		imageID, pullErr := runtime.Orchestrator.PullImage(ctx, containers.PullImageOptions{
			Image:                 localImageReference,
			AllowInsecureRegistry: true,
		})
		require.NoError(t, pullErr)
		require.NotEmpty(t, imageID)

		byName, inspectNameErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{localImageReference},
		})
		require.NoError(t, inspectNameErr)
		require.Len(t, byName, 1)
		require.NotEmpty(t, byName[0].Id)
		require.Contains(t, byName[0].Tags, localImageReference)
		for label, value := range tracker.MapLabels() {
			require.Equal(t, value, byName[0].Labels[label])
		}

		byID, inspectIDErr := runtime.Orchestrator.InspectImages(ctx, containers.InspectImagesOptions{
			Images: []string{imageID},
		})
		require.NoError(t, inspectIDErr)
		require.Len(t, byID, 1)
		require.Equal(t, byName[0].Id, byID[0].Id)
	})
}

func buildRegistryImage(
	t *testing.T,
	ctx context.Context,
	orchestrator containers.ContainerOrchestrator,
	tracker *containertest.ResourceTracker,
	registryBinaryPath string,
	image string,
) {
	t.Helper()

	contextDirectory := t.TempDir()
	binaryDestination := filepath.Join(contextDirectory, "oci-registry")
	require.NoError(t, copyTestTool(registryBinaryPath, binaryDestination))

	dockerfilePath := filepath.Join(contextDirectory, "Dockerfile")
	dockerfile := "FROM scratch\nCOPY --chmod=0755 oci-registry /oci-registry\nENTRYPOINT [\"/oci-registry\"]\n"
	require.NoError(t, usvc_io.WriteFile(dockerfilePath, []byte(dockerfile), osutil.PermissionOnlyOwnerReadWrite))

	buildErr := orchestrator.BuildImage(ctx, containers.BuildImageOptions{
		ContainerBuildContext: &containers.ContainerBuildContext{
			Context:    contextDirectory,
			Dockerfile: dockerfilePath,
			Tags:       []string{image},
			Labels:     tracker.Labels(),
		},
	})
	require.NoError(t, buildErr)
}
