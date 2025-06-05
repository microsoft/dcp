package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/health"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	internal_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func ensureContainerRunning(t *testing.T, ctx context.Context, container *apiv1.Container) (*apiv1.Container, containers.InspectedContainer) {
	return ensureContainerRunningEx(t, ctx, client, containerOrchestrator, container)
}

func ensureContainerRunningEx(t *testing.T, ctx context.Context, client ctrl_client.Client, co *ctrl_testutil.TestContainerOrchestrator, container *apiv1.Container) (*apiv1.Container, containers.InspectedContainer) {
	updated := ensureContainerState(t, ctx, client, container, apiv1.ContainerStateRunning)

	inspectedContainers, err := co.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updated.Status.ContainerID},
	})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspectedContainers, 1, "expected to find a single container")

	return updated, inspectedContainers[0]
}

func ensureContainerState(t *testing.T, ctx context.Context, client ctrl_client.Client, container *apiv1.Container, state apiv1.ContainerState) *apiv1.Container {
	updated := waitObjectAssumesStateEx(t, ctx, client, ctrl_client.ObjectKeyFromObject(container), func(currentContainer *apiv1.Container) (bool, error) {
		if currentContainer.Status.State == apiv1.ContainerStateFailedToStart {
			return false, fmt.Errorf("container creation failed: %s", currentContainer.Status.Message)
		}

		return currentContainer.Status.State == state, nil
	})

	return updated
}

func TestInvalidContainerName(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-container-name",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: "%%%INVALIDNAME%%%",
			Image:         "invalid-container-name-image",
		},
	}

	require.Len(t, ctr.Validate(ctx), 1, "Expected validation error for invalid container name")

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "valid-container-name",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: "a-1.2_34",
			Image:         "valid-container-name-image",
		},
	}

	require.Len(t, ctr.Validate(ctx), 0, "Unexpected validation error for valid container name")
}

func TestContainerLifecycleKey(t *testing.T) {
	t.Parallel()

	spec := apiv1.ContainerSpec{
		Env: []apiv1.EnvVar{
			{
				Name:  "A",
				Value: "A",
			},
			{
				Name:  "Z",
				Value: "Z",
			},
		},
	}

	lifecycleKey, isGenerated, hashErr := spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.True(t, isGenerated, "expected lifecycle key to be generated")
	require.NotEmpty(t, lifecycleKey, "expected lifecycle key to have a value")

	// Test with same env vars in different order
	equivalentSpec := apiv1.ContainerSpec{
		Env: []apiv1.EnvVar{
			{
				Name:  "Z",
				Value: "Z",
			},
			{
				Name:  "A",
				Value: "A",
			},
		},
	}

	equivalentLifecycleKey, equivalentIsGenerated, hashErr := equivalentSpec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.Equal(t, isGenerated, equivalentIsGenerated, "expected isGenerated to match")
	require.Equal(t, lifecycleKey, equivalentLifecycleKey, "expected lifecycle key to match")

	differentSpec := apiv1.ContainerSpec{
		Env: []apiv1.EnvVar{
			{
				Name:  "A",
				Value: "A",
			},
			{
				Name:  "H",
				Value: "H",
			},
			{
				Name:  "Z",
				Value: "Z",
			},
		},
	}

	differentLifecycleKey, _, hashErr := differentSpec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.NotEqual(t, lifecycleKey, differentLifecycleKey, "expected lifecycle key to be different")
}

func TestChangingDockerfileModifiesLifecycleKey(t *testing.T) {
	t.Parallel()

	const testName = "container-lifecycle-key-dockerfile"
	const imageName = testName + "-image"

	tmpDir, tmpErr := os.MkdirTemp(testutil.TestTempDir(), "lifecycle-dockerfile")
	require.NoError(t, tmpErr, "could not create temp dir")

	dockerfile := filepath.Join(tmpDir, "Dockerfile")

	t.Logf("Creating Dockerfile '%s'", dockerfile)
	writeErr := os.WriteFile(dockerfile, []byte("FROM scratch\n"), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr, "could not write Dockerfile")

	spec := apiv1.ContainerSpec{
		Image: imageName,
		Build: &apiv1.ContainerBuildContext{
			Context: tmpDir,
		},
	}

	lifecycleKey, isGenerated, hashErr := spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.True(t, isGenerated, "expected lifecycle key to be generated")
	require.NotEmpty(t, lifecycleKey, "expected lifecycle key to have a value")

	t.Logf("Changing Dockerfile '%s'", dockerfile)
	writeErr = os.WriteFile(dockerfile, []byte("FROM scratch\nRUN echo hello"), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr, "could not write Dockerfile")

	newLifecycleKey, _, hashErr := spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.NotEqual(t, lifecycleKey, newLifecycleKey, "expected lifecycle key to be different")

	// Repeat the test with a different context path
	t.Logf("Testing with new context path")
	tmpDir, tmpErr = os.MkdirTemp(testutil.TestTempDir(), "lifecycle-dockerfile")
	require.NoError(t, tmpErr, "could not create temp dir")

	dockerfile = filepath.Join(tmpDir, "Dockerfile")

	t.Logf("Creating Dockerfile '%s'", dockerfile)
	writeErr = os.WriteFile(dockerfile, []byte("FROM scratch\n"), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr, "could not write dockerfile")

	spec.Build.Context = tmpDir

	newContextLifecycleKey, _, hashErr := spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.NotEqual(t, lifecycleKey, newContextLifecycleKey, "expected lifecycle keys to be different due to context path")

	t.Logf("Changing Dockerfile '%s'", dockerfile)
	writeErr = os.WriteFile(dockerfile, []byte("FROM scratch\nRUN echo hello"), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr, "could not write Dockerfile")

	newLifecycleKey, _, hashErr = spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.NotEqual(t, newContextLifecycleKey, newLifecycleKey, "expected lifecycle key to be different")

	// Repeat the test with an explicitly set Dockerfile name
	dockerfile = filepath.Join(tmpDir, "MyDockerfile")

	t.Logf("Creating MyDockerfile '%s'", dockerfile)
	writeErr = os.WriteFile(dockerfile, []byte("FROM scratch\n"), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr, "could not write dockerfile")

	spec.Build.Dockerfile = "./MyDockerfile"

	customDockerfileLifecycleKey, _, hashErr := spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.Equal(t, newContextLifecycleKey, customDockerfileLifecycleKey, "expected lifecycle keys for same Dockerfile content to be equal for same context path")

	t.Logf("Changing dockerfile '%s'", dockerfile)
	writeErr = os.WriteFile(dockerfile, []byte("FROM scratch\nRUN echo hello"), osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, writeErr, "could not write Dockerfile")

	newMyDockerfileLifecycleKey, _, hashErr := spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.Equal(t, newLifecycleKey, newMyDockerfileLifecycleKey, "expected lifecycle keys for same Dockerfile content to be equal for same context path")
}

// Ensure a container instance is started when new Container object appears
func TestContainerInstanceStarts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-instance-starts"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, _ = ensureContainerRunning(t, ctx, &ctr)
}

// Ensure a container instance is started after container runtime goes from unhealthy to healthy
func TestContainerRuntimeUnhealthy(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)

	const testName = "container-runtime-unhealthy"
	const imageName = testName + "-image"

	log := testutil.NewLogForTesting(t.Name())

	// We are going to use a separate instance of the API server because we need to simulate container runtime being unhealthy,
	// and that might interfere with other tests if we used the shared container orchestrator.

	serverInfo, _, _, startupErr := StartTestEnvironment(ctx, ContainerController, t.Name(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Setting container runtime to unhealthy...")
	serverInfo.ContainerOrchestrator.SetRuntimeHealth(false)

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	t.Logf("Ensure Container '%s' state is 'runtime unhealthy'...", ctr.ObjectMeta.Name)
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateRuntimeUnhealthy, nil
	})

	t.Logf("Setting container runtime to healthy...")
	serverInfo.ContainerOrchestrator.SetRuntimeHealth(true)

	t.Logf("Ensure Container '%s' is running...", ctr.ObjectMeta.Name)
	_, _ = ensureContainerRunningEx(t, ctx, serverInfo.Client, serverInfo.ContainerOrchestrator, &ctr)
}

func TestContainerMarkedDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-marked-done"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	updatedContainer, _ := ensureContainerRunning(t, ctx, &ctr)

	err = containerOrchestrator.SimulateContainerExit(ctx, updatedContainer.Status.ContainerID, 0)
	require.NoError(t, err, "could not simulate container exit")

	t.Log("Ensure Container object status reflects the state of the running container...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return !c.Status.FinishTimestamp.IsZero() && c.Status.State == apiv1.ContainerStateExited, nil
	})
}

func TestContainerStartupFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-startup-failure"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	// Cause the orchestrator to fail to create the container
	errMsg := fmt.Sprintf("Simulating Container '%s' startup failure...", testName)
	containerOrchestrator.FailMatchingContainers(ctx, testName, 1, errMsg)

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	// The container should be marked as "failed to start", and the Message property of its status
	// should contain the error from container orchestrator, i.e. Docker
	t.Log("Ensure container state is 'failed to start'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateFailedToStart
		messageOK := strings.Contains(c.Status.Message, errMsg)
		unhealthy := c.Status.HealthStatus == apiv1.HealthStatusUnhealthy
		return statusUpdated && messageOK && unhealthy, nil
	})
}

func validatePorts(t *testing.T, inspected containers.InspectedContainer, ports []apiv1.ContainerPort) {
	for _, port := range ports {
		protocol := port.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		mappings, found := inspected.Ports[fmt.Sprintf("%d/%s", port.ContainerPort, protocol)]
		require.True(t, found, "container port %d/%s was not published", port.ContainerPort, port.Protocol)
		require.Len(t, mappings, 1, "expected a single mapping for container port %d", port.ContainerPort)

		if port.HostPort != 0 {
			require.Equal(t, fmt.Sprintf("%d", port.HostPort), mappings[0].HostPort, "expected the host port to be %s", port.HostPort)
		} else {
			hostPort, hostPortErr := strconv.Atoi(mappings[0].HostPort)
			require.NoError(t, hostPortErr, "expected the host port to be a number")
			require.GreaterOrEqual(t, hostPort, ctrl_testutil.MinRandomHostPort)
			require.Less(t, hostPort, ctrl_testutil.MaxRandomHostPort)
		}

		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = networking.IPv4LocalhostDefaultAddress
		}
		require.Equal(t, hostIP, mappings[0].HostIp, "expected the host IP to be %s", hostIP)
	}

	require.Len(t, maps.Keys(inspected.Ports), len(ports), "did not find expected number of ports")
}

// If ports are part of the spec, they are published to the host
func TestContainerStartWithPorts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	// Case 1: just ContainerPort
	testName := "container-start-with-ports-case1"
	imageName := testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345},
				{ContainerPort: 3456},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspected := ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 2: ContainerPort and HostPort
	testName = "container-start-with-ports-case2"
	imageName = testName + "-image"

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostPort: 8885},
				{ContainerPort: 3456, HostPort: 8886},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 3: ContainerPort and HostIP
	testName = "container-start-with-ports-case3"
	imageName = testName + "-image"

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostIP: "127.0.2.3"},
				{ContainerPort: 3456, HostIP: "127.0.2.4"},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 4: ContainerPort, HostIP, and Protocol
	testName = "container-start-with-ports-case4"
	imageName = testName + "-image"

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostIP: "127.0.3.4", Protocol: "tcp"},
				{ContainerPort: 3456, HostIP: "127.0.4.4", Protocol: "udp"},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)

	// Case 5: ContainerPort, HostIP, HostPort, and Protocol
	testName = "container-start-with-ports-case5"
	imageName = testName + "-image"

	ctr = apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{ContainerPort: 2345, HostPort: 12202, HostIP: "127.0.3.4", Protocol: "tcp"},
				{ContainerPort: 3456, HostPort: 12205, HostIP: "127.0.4.4", Protocol: "udp"},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspected = ensureContainerRunning(t, ctx, &ctr)

	validatePorts(t, inspected, ctr.Spec.Ports)
}

func TestContainerDelayStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-delay-start-state"
	const imageName = testName + "-image"

	shouldStart := false
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Start: &shouldStart,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	t.Logf("Ensure Container '%s' state is 'starting'...", ctr.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateEmpty || c.Status.State == apiv1.ContainerStateStarting, nil
	})

	t.Logf("Waiting 5 seconds")
	time.Sleep(5 * time.Second)

	t.Logf("Ensure Container '%s' state is still 'starting'...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateEmpty || c.Status.State == apiv1.ContainerStateStarting, nil
	})

	shouldStart = true
	t.Logf("Patching Container '%s' to start...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Start = &shouldStart
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Logf("Ensure Container '%s' state is 'running'...", ctr.ObjectMeta.Name)
	updatedCtr, _ = ensureContainerRunning(t, ctx, updatedCtr)

	t.Logf("Stopping Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Stop = true
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Log("Ensure container state is 'Exited'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited, nil
	})

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server, '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err = containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.Error(t, err, "expected the container to be gone")
	require.Len(t, inspected, 0, "expected the container to be gone")
}

func TestNoExistingPersistentContainerDelayStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "no-existing-persistent-container-delay-start-state"
	const imageName = testName + "-image"

	shouldStart := false
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
			Start:         &shouldStart,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	t.Logf("Ensure Container '%s' state is 'starting'...", ctr.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateEmpty || c.Status.State == apiv1.ContainerStateStarting, nil
	})

	t.Logf("Waiting 5 seconds")
	time.Sleep(5 * time.Second)

	t.Logf("Ensure Container '%s' state is still 'starting'...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return (c.Status.State == apiv1.ContainerStateEmpty || c.Status.State == apiv1.ContainerStateStarting) && c.Status.LifecycleKey != "", nil
	})

	initialLifecycleKey := updatedCtr.Status.LifecycleKey

	shouldStart = true
	t.Logf("Patching Container '%s' to start...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Start = &shouldStart
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Logf("Ensure Container '%s' state is 'running'...", ctr.ObjectMeta.Name)
	updatedCtr, inspectedCtr := ensureContainerRunning(t, ctx, updatedCtr)
	require.Equal(t, inspectedCtr.Status, containers.ContainerStatusRunning, "expected the container to be in 'running' state")

	require.Equal(t, initialLifecycleKey, updatedCtr.Status.LifecycleKey, "reported lifecycle key should not change")

	calculatedLifecycleKey, _, hashErr := updatedCtr.Spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.Equal(t, initialLifecycleKey, calculatedLifecycleKey, "calculated lifecycle key should not change")

	t.Logf("Stopping Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Stop = true
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Log("Ensure container state is 'Exited'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited, nil
	})

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server, '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err = containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected the container to be gone")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")
}

func TestExistingPersistentContainerDelayStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "existing-persistent-container-delay-start-state"
	const imageName = testName + "-image"

	shouldStart := false
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
			Start:         &shouldStart,
		},
	}

	lifecycleKey, _, hashErr := ctr.Spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")

	createSpec := ctr.Spec
	createSpec.Labels = []apiv1.ContainerLabel{
		{
			Key:   "com.microsoft.developer.usvc-dev.build",
			Value: "test",
		},
		{
			Key:   "com.microsoft.developer.usvc-dev.lifecycle-key",
			Value: lifecycleKey,
		},
	}

	id, err := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:          testName,
		ContainerSpec: createSpec,
	})
	require.NoError(t, err, "could not create container resource")

	_, err = containerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{id},
	})
	require.NoError(t, err, "could not start container resource")

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	t.Logf("Ensure Container '%s' state is 'running'...", ctr.ObjectMeta.Name)
	updatedCtr, inspectedCtr := ensureContainerRunning(t, ctx, &ctr)
	require.Equal(t, inspectedCtr.Status, containers.ContainerStatusRunning, "expected the container to be in 'running' state")

	require.Equal(t, id, inspectedCtr.Id, "container ID should match the one created by the orchestrator")

	require.Equal(t, lifecycleKey, updatedCtr.Status.LifecycleKey, "reported lifecycle key should not change")

	calculatedLifecycleKey, _, hashErr := updatedCtr.Spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.Equal(t, lifecycleKey, calculatedLifecycleKey, "calculated lifecycle key should not change")

	shouldStart = true
	t.Logf("Patching Container '%s' to start...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Start = &shouldStart
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Logf("Ensure Container '%s' state is 'running'...", ctr.ObjectMeta.Name)
	updatedCtr, inspectedCtr = ensureContainerRunning(t, ctx, &ctr)
	require.Equal(t, inspectedCtr.Status, containers.ContainerStatusRunning, "expected the container to be in 'running' state")

	require.Equal(t, lifecycleKey, updatedCtr.Status.LifecycleKey, "reported lifecycle key should not change")

	calculatedLifecycleKey, _, hashErr = updatedCtr.Spec.GetLifecycleKey()
	require.NoError(t, hashErr, "expected no error when generating lifecycle key")
	require.Equal(t, lifecycleKey, calculatedLifecycleKey, "calculated lifecycle key should not change")

	t.Logf("Stopping Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Stop = true
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Log("Ensure container state is 'Exited'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited, nil
	})

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server, '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err = containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected the container to be gone")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")
}

func TestContainerStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-stop-state"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Stopping Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		containerPatch.Spec.Stop = true
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, err, "Container object could not be patched")

	t.Log("Ensure container state is 'Exited'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited, nil
	})

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "could not inspect the container")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, containers.ContainerStatusExited, inspected[0].Status, "expected the container to be in 'exited' state")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server, '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err = containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.Error(t, err, "expected the container to be gone")
	require.Len(t, inspected, 0, "expected the container to be gone")
}

func TestContainerDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-deletion"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	// Subscribe to container events to verify that the container is deleted gracefully
	ctrEventCh := concurrency.NewUnboundedChanBuffered[containers.EventMessage](ctx, 2, 2)
	ctrEventSub, watchErr := containerOrchestrator.WatchContainers(ctrEventCh.In)
	require.NoError(t, watchErr, "could not subscribe to container events")
	defer ctrEventSub.Cancel()

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	t.Logf("Ensure that the Container '%s' is stopped and removed gracefully...", ctr.ObjectMeta.Name)
	stopCount := 0
	removeCount := 0

readEvents:
	for {
		select {
		case event, isOpen := <-ctrEventCh.Out:
			if !isOpen {
				t.Fatal("container event channel was closed unexpectedly")
			}

			if event.Actor.ID != updatedCtr.Status.ContainerID {
				break
			}

			// Note: containers.EventActionStop is raised when the container is killed,
			// so it is not a good indicator that the container is being stopped gracefully.

			switch event.Action {
			case ctrl_testutil.TestEventActionStopWithoutRemove:
				stopCount++
			case containers.EventActionDestroy:
				removeCount++
			}

			if stopCount == 1 && removeCount == 1 {
				t.Logf("Container '%s' was stopped and removed gracefully", ctr.ObjectMeta.Name)
				break readEvents
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for container events")
		}
	}
}

func TestContainerRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	// If a container shuts down and then restarts, it should be tracked as running
	const testName = "container-restart"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)
	err = containerOrchestrator.SimulateContainerExit(ctx, updatedCtr.Status.ContainerID, 0)
	require.NoError(t, err, "could not simulate container exit")

	t.Log("Ensure container state is 'stopped'...")
	updatedCtr = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateExited && c.Status.ExitCode != apiv1.UnknownExitCode && *c.Status.ExitCode == 0
		return statusUpdated, nil
	})

	_, err = containerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "could not simulate container start")

	t.Log("Ensure container state is 'running'...")
	_, _ = ensureContainerRunning(t, ctx, &ctr)

	originalFinishedAt := updatedCtr.Status.FinishTimestamp

	err = containerOrchestrator.SimulateContainerExit(ctx, updatedCtr.Status.ContainerID, -1)
	require.NoError(t, err, "could not simulate container exit")

	t.Log("Ensure container state is 'stopped'...")
	updatedCtr = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateExited && c.Status.ExitCode != apiv1.UnknownExitCode && *c.Status.ExitCode == -1
		return statusUpdated, nil
	})

	require.True(t, originalFinishedAt.Before(&updatedCtr.Status.FinishTimestamp))
}

func TestContainerMultipleServingPortsInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const IPAddr = "127.22.132.13"
	const testName = "test-container-multiple-serving-ports-injected"
	services := map[string]apiv1.Service{
		"svc-a": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      testName + "-svc-a",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11760,
			},
		},
		"svc-b": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      testName + "-svc-b",
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol: apiv1.TCP,
				Address:  IPAddr,
				Port:     11761,
			},
		},
	}

	for _, svc := range services {
		t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
		err := client.Create(ctx, &svc)
		require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)
	}

	const svcAHostPort = 11770
	const svcBContainerPort = 11771
	var spAnn strings.Builder
	spAnn.WriteString("[")

	// Injected via env var, matched by host port
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s", "port":%d}`, services["svc-a"].ObjectMeta.Name, svcAHostPort))
	spAnn.WriteString(",")

	// Injected via startup parameter, matched by container port
	spAnn.WriteString(fmt.Sprintf(`{"serviceName":"%s","port":%d}`, services["svc-b"].ObjectMeta.Name, svcBContainerPort))

	spAnn.WriteString("]")

	const svcAContainerPort = 2345
	const imageName = testName + "-image"
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName,
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": spAnn.String()},
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Ports: []apiv1.ContainerPort{
				{HostPort: svcAHostPort, ContainerPort: svcAContainerPort, HostIP: IPAddr, Protocol: "tcp"},
				{ContainerPort: svcBContainerPort, HostIP: IPAddr, Protocol: "tcp"},
			},
			Env: []apiv1.EnvVar{
				{
					Name:  "SVC_A_PORT",
					Value: fmt.Sprintf(`{{- portForServing "%s" -}}`, services["svc-a"].ObjectMeta.Name),
				},
			},
			Args: []string{
				fmt.Sprintf(`--svc-b-port={{- portForServing "%s" -}}`, services["svc-b"].ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Container '%s'...", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	_, inspected := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Ensure that Container '%s' is started with the expected ports, env vars, and startup args...", ctr.ObjectMeta.Name)
	expectedArg := fmt.Sprintf("--svc-b-port=%d", svcBContainerPort)
	require.Contains(t, inspected.Args, expectedArg, "expected the container to have the startup arg %s", expectedArg)

	expectedEnvVar := fmt.Sprintf("SVC_A_PORT=%d", svcAContainerPort)
	require.Equal(t, fmt.Sprintf("%d", svcAContainerPort), inspected.Env["SVC_A_PORT"], "expected the container to have the env var %s", expectedEnvVar)

	validatePorts(t, inspected, []apiv1.ContainerPort{
		{ContainerPort: svcAContainerPort, HostPort: svcAHostPort, HostIP: IPAddr, Protocol: "tcp"},
		{ContainerPort: svcBContainerPort, HostIP: IPAddr, Protocol: "tcp"},
	})

	t.Logf("Ensure the Status.EffectiveEnv for Container '%s' contains the injected ports...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		return len(currentCtr.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedCtr.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Container '%s' effective environment does not contain expected port information for service A. The effective environemtn is %v", ctr.ObjectMeta.Name, effectiveEnv)

	t.Logf("Ensure the Status.EffectiveArgs for Container '%s' contains the injected port...", ctr.ObjectMeta.Name)
	require.Equal(t, updatedCtr.Status.EffectiveArgs[0], expectedArg, "The Container '%s' startup parameters do not include expected port for service B. The startup parameters are %v", ctr.ObjectMeta.Name, updatedCtr.Status.EffectiveArgs)

	t.Logf("Ensure services exposed by Container '%s' get to Ready state...", ctr.ObjectMeta.Name)
	for _, svc := range services {
		waitServiceReady(t, ctx, &svc)
	}
}

func TestContainerServingAddressInjected(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-container-serving-address-injected"
	const ServiceIPAddr = "127.63.29.2"
	const ContainerIPAddr = "127.63.29.3"

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-svc",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  ServiceIPAddr,
			Port:     26003,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)

	const ContainerPort = 26004
	var spAnn strings.Builder
	spAnn.WriteString("[")
	spAnn.WriteString(fmt.Sprintf(`{ "serviceName":"%s", "address": "%s", "port": %d }`, svc.ObjectMeta.Name, ContainerIPAddr, ContainerPort))
	spAnn.WriteString("]")

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testName + "-server",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": spAnn.String()},
		},
		Spec: apiv1.ContainerSpec{
			Image: testName + "-image",
			Ports: []apiv1.ContainerPort{
				{ContainerPort: ContainerPort},
			},
			Env: []apiv1.EnvVar{
				{
					Name:  "SERVICE_ADDRESS",
					Value: fmt.Sprintf(`{{- addressFor "%s" -}}`, svc.ObjectMeta.Name),
				},
			},
			Args: []string{
				fmt.Sprintf(`--serving-address={{- addressForServing "%s" -}}`, svc.ObjectMeta.Name),
			},
		},
	}

	t.Logf("Creating Container '%s'...", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Ensure that Container '%s' is started with the expected ports, env vars, and startup args...", ctr.ObjectMeta.Name)
	expectedArg := fmt.Sprintf("--serving-address=%s", ContainerIPAddr)
	expectedEnvVar := fmt.Sprintf("SERVICE_ADDRESS=%s", ServiceIPAddr)

	_, inspected := ensureContainerRunning(t, ctx, &ctr)
	require.Contains(t, inspected.Args, expectedArg, "expected the container to have the startup arg %s", expectedArg)
	require.Equal(t, ServiceIPAddr, inspected.Env["SERVICE_ADDRESS"], "expected the container to have the env var %s", expectedEnvVar)
	validatePorts(t, inspected, []apiv1.ContainerPort{
		{ContainerPort: ContainerPort, HostIP: networking.IPv4LocalhostDefaultAddress},
	})

	t.Logf("Ensure the Status.EffectiveEnv for Container '%s' contains the injected address information...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		return len(currentCtr.Status.EffectiveEnv) > 0, nil
	})
	effectiveEnv := slices.Map[apiv1.EnvVar, string](updatedCtr.Status.EffectiveEnv, func(v apiv1.EnvVar) string {
		return fmt.Sprintf("%s=%s", v.Name, v.Value)
	})
	require.True(t, slices.Contains(effectiveEnv, expectedEnvVar), "The Container '%s' effective environment does not contain expected address information for service '%s'. The effective environemtn is %v", ctr.ObjectMeta.Name, svc.ObjectMeta.Name, effectiveEnv)

	t.Logf("Ensure the Status.EffectiveArgs for Container '%s' contains the injected address information...", ctr.ObjectMeta.Name)
	require.Equal(t, updatedCtr.Status.EffectiveArgs[0], expectedArg, "The Container '%s' startup parameters do not include expected address information for service '%s'. The startup parameters are %v", ctr.ObjectMeta.Name, svc.ObjectMeta.Name, updatedCtr.Status.EffectiveArgs)

	t.Logf("Ensure service exposed by Container '%s' gets to Ready state...", ctr.ObjectMeta.Name)
	waitServiceReady(t, ctx, &svc)
}

func TestPersistentContainerDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-deletion"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")
	require.Equal(t, inspected[0].Status, containers.ContainerStatusRunning, "expected the container to be running")
}

func TestPersistentContainerAlreadyExists(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-already-exists"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
		},
	}

	id, err := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:          testName,
		ContainerSpec: ctr.Spec,
	})
	require.NoError(t, err, "could not create container resource")

	_, err = containerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{id},
	})
	require.NoError(t, err, "could not start container resource")

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	require.Equal(t, id, updatedCtr.Status.ContainerID, "container ID does not match existing value")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")
}

func TestPersistentContainerAlreadyExistsSameLifecycleKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-already-exists-same-lifecycle-key"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
			LifecycleKey:  "testkey",
		},
	}

	createSpec := ctr.Spec
	createSpec.Labels = []apiv1.ContainerLabel{
		{
			Key:   "com.microsoft.developer.usvc-dev.build",
			Value: "test",
		},
		{
			Key:   "com.microsoft.developer.usvc-dev.lifecycle-key",
			Value: ctr.Spec.LifecycleKey,
		},
	}

	id, err := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:          testName,
		ContainerSpec: createSpec,
	})
	require.NoError(t, err, "could not create container resource")

	_, err = containerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{id},
	})
	require.NoError(t, err, "could not start container resource")

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	require.Equal(t, id, updatedCtr.Status.ContainerID, "container ID does not match existing value")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")
}

func TestPersistentContainerAlreadyExistsDifferentLifecycleKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-already-exists-different-lifecycle-key"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
			LifecycleKey:  "newtestkey",
		},
	}

	createSpec := ctr.Spec
	createSpec.Labels = []apiv1.ContainerLabel{
		{
			Key:   "com.microsoft.developer.usvc-dev.build",
			Value: "test",
		},
		{
			Key:   "com.microsoft.developer.usvc-dev.lifecycle-key",
			Value: "oldtestkey",
		},
	}

	id, err := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:          testName,
		ContainerSpec: createSpec,
	})
	require.NoError(t, err, "could not create container resource")

	_, err = containerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{id},
	})
	require.NoError(t, err, "could not start container resource")

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	require.NotEqual(t, id, updatedCtr.Status.ContainerID, "container ID matches existing value with changed lifecycle key")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")
}

// Ensure a container instance is started when new Container object appears
func TestContainerWithBuildContextInstanceStarts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-with-build-context-instance-starts"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Build: &apiv1.ContainerBuildContext{
				Context:    ".",
				Dockerfile: "./Dockerfile",
			},
			Image: imageName,
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, _ = ensureContainerRunning(t, ctx, &ctr)

	require.True(t, containerOrchestrator.HasImage(ctr.SpecifiedImageNameOrDefault()), "expected image to be present in the orchestrator")
	_, found := containerOrchestrator.GetImageId(ctr.SpecifiedImageNameOrDefault())
	require.True(t, found, "expected image ID to be found")
}

func TestPersistentContainerWithBuildContextAlreadyExists(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "persistent-container-with-build-context-already-exists"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Build: &apiv1.ContainerBuildContext{
				Context:    ".",
				Dockerfile: "./Dockefile",
			},
			Image:         imageName,
			ContainerName: testName,
			Persistent:    true,
		},
	}

	id, err := containerOrchestrator.CreateContainer(ctx, containers.CreateContainerOptions{
		Name:          testName,
		ContainerSpec: ctr.Spec,
	})
	require.NoError(t, err, "could not create container resource")

	_, err = containerOrchestrator.StartContainers(ctx, containers.StartContainersOptions{
		Containers: []string{id},
	})
	require.NoError(t, err, "could not start container resource")

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	require.Equal(t, id, updatedCtr.Status.ContainerID, "container ID does not match existing value")

	t.Logf("Deleting Container object '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "container object could not be deleted")

	t.Logf("Ensure that Container object really disappeared from the API server '%s'...", ctr.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &ctr)

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.NoError(t, err, "expected to find a container")
	require.Len(t, inspected, 1, "expected to find a single container")

	require.False(t, containerOrchestrator.HasImage(ctr.SpecifiedImageNameOrDefault()), "image should only be built if the persistent container doesn't exist")
}

func TestContainerStateBecomesUnknownIfContainerResourceDeleted(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-stete-becomes-unknown-after-deletion"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Deleting Container resource '%s'...", updatedCtr.Status.ContainerID)
	_, err = containerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
		Force:      true,
	})
	require.NoError(t, err, "could not remove container resource '%s'", updatedCtr.Status.ContainerID)

	t.Logf("Ensure Container object status becomes 'Unknown'...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateUnknown && c.Status.HealthStatus == apiv1.HealthStatusCaution, nil
	})
}

// Verify that stdout and stderr logs can be captured in non-follow mode.
// The sub-tests are verifying that the logs can be obtained when Container is running,
// and when it has finished running.
func TestContainerLogsNonFollow(t *testing.T) {
	t.Parallel()

	type testcase struct {
		description         string
		opts                apiv1.LogOptions
		ensureDesiredState  func(*testing.T, context.Context, *apiv1.Container)
		expectedStdoutLines [][]byte
		expectedStderrLines [][]byte
	}

	const testName = "test-container-logs-non-follow"

	ensureRunning := func(t *testing.T, ctx context.Context, c *apiv1.Container) {
		require.True(t, c.Status.State == apiv1.ContainerStateRunning)
	}
	transitionFromRunningToExited := func(t *testing.T, ctx context.Context, c *apiv1.Container) {
		require.True(t, c.Status.State == apiv1.ContainerStateRunning)
		exitErr := containerOrchestrator.SimulateContainerExit(ctx, c.Status.ContainerID, 0)
		require.NoError(t, exitErr)
		_ = ensureContainerState(t, ctx, client, c, apiv1.ContainerStateExited)
	}

	const LINE_COUNT = 20
	stdoutLines := generateLogLines([]byte("stdout"), LINE_COUNT)
	stderrLines := generateLogLines([]byte("stderr"), LINE_COUNT)
	var skip int64 = 3
	var limit int64 = 5
	var tail int64 = 7 // Must be greater than limit to test tail+limit combination.

	testcases := []testcase{
		{
			description:         "running",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: stdoutLines,
			expectedStderrLines: stderrLines,
		},
		{
			description:         "finished",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: stdoutLines,
			expectedStderrLines: stderrLines,
		},
		{
			description:         "running-ts",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withTimestampRegexes(stdoutLines),
			expectedStderrLines: withTimestampRegexes(stderrLines),
		},
		{
			description:         "finished-ts",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withTimestampRegexes(stdoutLines),
			expectedStderrLines: withTimestampRegexes(stderrLines),
		},
		{
			description:         "running-skip",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: stdoutLines[skip:],
			expectedStderrLines: stderrLines[skip:],
		},
		{
			description:         "finished-skip",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: stdoutLines[skip:],
			expectedStderrLines: stderrLines[skip:],
		},
		{
			description:         "running-ts-skip",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[skip:],
			expectedStderrLines: withTimestampRegexes(stderrLines)[skip:],
		},
		{
			description:         "finished-ts-skip",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[skip:],
			expectedStderrLines: withTimestampRegexes(stderrLines)[skip:],
		},
		{
			description:         "running-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: stdoutLines[:limit],
			expectedStderrLines: stderrLines[:limit],
		},
		{
			description:         "finished-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: stdoutLines[:limit],
			expectedStderrLines: stderrLines[:limit],
		},
		{
			description:         "running-ts-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[:limit],
			expectedStderrLines: withTimestampRegexes(stderrLines)[:limit],
		},
		{
			description:         "finished-ts-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[:limit],
			expectedStderrLines: withTimestampRegexes(stderrLines)[:limit],
		},
		{
			description:         "running-skip-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip, Limit: &limit},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: stdoutLines[skip:][:limit],
			expectedStderrLines: stderrLines[skip:][:limit],
		},
		{
			description:         "finished-skip-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip, Limit: &limit},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: stdoutLines[skip:][:limit],
			expectedStderrLines: stderrLines[skip:][:limit],
		},
		{
			description:         "running-ts-skip-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip, Limit: &limit},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[skip:][:limit],
			expectedStderrLines: withTimestampRegexes(stderrLines)[skip:][:limit],
		},
		{
			description:         "finished-ts-skip-limit",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip, Limit: &limit},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[skip:][:limit],
			expectedStderrLines: withTimestampRegexes(stderrLines)[skip:][:limit],
		},
		{
			description:         "running-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Tail: &tail},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: stdoutLines[len(stdoutLines)-int(tail):],
			expectedStderrLines: stderrLines[len(stderrLines)-int(tail):],
		},
		{
			description:         "finished-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Tail: &tail},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: stdoutLines[len(stdoutLines)-int(tail):],
			expectedStderrLines: stderrLines[len(stderrLines)-int(tail):],
		},
		{
			description:         "running-ts-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Tail: &tail},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[len(stdoutLines)-int(tail):],
			expectedStderrLines: withTimestampRegexes(stderrLines)[len(stderrLines)-int(tail):],
		},
		{
			description:         "finished-ts-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Tail: &tail},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[len(stdoutLines)-int(tail):],
			expectedStderrLines: withTimestampRegexes(stderrLines)[len(stderrLines)-int(tail):],
		},
		{
			description:         "running-limit-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit, Tail: &tail},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: stdoutLines[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: stderrLines[len(stderrLines)-int(tail):][:limit],
		},
		{
			description:         "finished-limit-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit, Tail: &tail},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: stdoutLines[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: stderrLines[len(stderrLines)-int(tail):][:limit],
		},
		{
			description:         "running-ts-limit-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit, Tail: &tail},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: withTimestampRegexes(stderrLines)[len(stderrLines)-int(tail):][:limit],
		},
		{
			description:         "finished-ts-limit-tail",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit, Tail: &tail},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withTimestampRegexes(stdoutLines)[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: withTimestampRegexes(stderrLines)[len(stderrLines)-int(tail):][:limit],
		},
		{
			description:         "running-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines),
			expectedStderrLines: withLineNumberRegexes(stderrLines),
		},
		{
			description:         "finished-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines),
			expectedStderrLines: withLineNumberRegexes(stderrLines),
		},
		{
			description:         "running-ts-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines)),
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines)),
		},
		{
			description:         "finished-ts-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines)),
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines)),
		},
		{
			description:         "running-skip-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[skip:],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[skip:],
		},
		{
			description:         "finished-skip-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[skip:],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[skip:],
		},
		{
			description:         "running-ts-skip-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[skip:],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[skip:],
		},
		{
			description:         "finished-ts-skip-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[skip:],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[skip:],
		},
		{
			description:         "running-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[:limit],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[:limit],
		},
		{
			description:         "finished-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[:limit],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[:limit],
		},
		{
			description:         "running-ts-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[:limit],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[:limit],
		},
		{
			description:         "finished-ts-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[:limit],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[:limit],
		},
		{
			description:         "running-skip-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[skip:][:limit],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[skip:][:limit],
		},
		{
			description:         "finished-skip-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Skip: &skip, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[skip:][:limit],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[skip:][:limit],
		},
		{
			description:         "running-ts-skip-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[skip:][:limit],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[skip:][:limit],
		},
		{
			description:         "finished-ts-skip-limit-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Skip: &skip, Limit: &limit, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[skip:][:limit],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[skip:][:limit],
		},
		{
			description:         "running-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[len(stdoutLines)-int(tail):],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[len(stdoutLines)-int(tail):],
		},
		{
			description:         "finished-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[len(stdoutLines)-int(tail):],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[len(stdoutLines)-int(tail):],
		},
		{
			description:         "running-ts-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[len(stdoutLines)-int(tail):],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[len(stdoutLines)-int(tail):],
		},
		{
			description:         "finished-ts-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[len(stdoutLines)-int(tail):],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[len(stdoutLines)-int(tail):],
		},
		{
			description:         "running-limit-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[len(stdoutLines)-int(tail):][:limit],
		},
		{
			description:         "finished-limit-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: false, Limit: &limit, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(stdoutLines)[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: withLineNumberRegexes(stderrLines)[len(stdoutLines)-int(tail):][:limit],
		},
		{
			description:         "running-ts-limit-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  ensureRunning,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[len(stdoutLines)-int(tail):][:limit],
		},
		{
			description:         "finished-ts-limit-tail-line-numbers",
			opts:                apiv1.LogOptions{Follow: false, Timestamps: true, Limit: &limit, Tail: &tail, LineNumbers: true},
			ensureDesiredState:  transitionFromRunningToExited,
			expectedStdoutLines: withLineNumberRegexes(withTimestampRegexes(stdoutLines))[len(stdoutLines)-int(tail):][:limit],
			expectedStderrLines: withLineNumberRegexes(withTimestampRegexes(stderrLines))[len(stdoutLines)-int(tail):][:limit],
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			containerName := testName + "-" + tc.description

			ctr := apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{
					Name:      containerName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ContainerSpec{
					Image: containerName + "-image",
				},
			}

			t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
			err := client.Create(ctx, &ctr)
			require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

			updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

			t.Logf("Simulating logging for Container '%s'...", ctr.ObjectMeta.Name)
			for i := 0; i < LINE_COUNT; i++ {
				logErr := containerOrchestrator.SimulateContainerLogging(updatedCtr.Status.ContainerID, apiv1.LogStreamSourceStdout, osutil.WithNewline(stdoutLines[i]))
				require.NoError(t, logErr, "could not simulate logging to stdout for Container '%s'", containerName)
				logErr = containerOrchestrator.SimulateContainerLogging(updatedCtr.Status.ContainerID, apiv1.LogStreamSourceStderr,
					osutil.WithNewline(stderrLines[i]))
				require.NoError(t, logErr, "could not simulate logging to stderr for Container '%s'", containerName)
			}

			t.Logf("Transitioning Container '%s' to desired state...", ctr.ObjectMeta.Name)
			tc.ensureDesiredState(t, ctx, updatedCtr)

			t.Logf("Ensure logs can be captured for Container '%s'...", ctr.ObjectMeta.Name)

			opts := tc.opts
			opts.Source = "stdout"
			waitErr := waitForObjectLogs(ctx, &ctr, opts, tc.expectedStdoutLines, nil)
			require.NoError(t, waitErr)

			opts = tc.opts
			opts.Source = "stderr"
			waitErr = waitForObjectLogs(ctx, &ctr, opts, tc.expectedStderrLines, nil)
			require.NoError(t, waitErr)
		})
	}
}

// Verify that logs can be captured in follow mode when log stream is open before any logs are written.
func TestContainerLogsFollowFromStart(t *testing.T) {
	t.Parallel()

	type testcase struct {
		description   string
		opts          apiv1.LogOptions
		expectedLines [][]byte
	}

	const testName = "test-container-logs-follow-from-start"

	const LINE_COUNT = 11
	lines := generateLogLines([]byte("stdout"), LINE_COUNT)
	var skip int64 = 2
	var tail int64 = 5

	testcases := []testcase{
		{
			description:   "regular",
			opts:          apiv1.LogOptions{Follow: true, Timestamps: false},
			expectedLines: lines,
		},
		{
			description:   "skip",
			opts:          apiv1.LogOptions{Follow: true, Timestamps: false, Skip: &skip},
			expectedLines: lines[skip:],
		},
		{
			description:   "tail",
			opts:          apiv1.LogOptions{Follow: true, Timestamps: false, Tail: &tail},
			expectedLines: lines[len(lines)-int(tail):],
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			containerName := testName + "-" + tc.description

			ctr := apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{
					Name:      containerName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ContainerSpec{
					Image: containerName + "-image",
				},
			}

			t.Logf("Creating Container '%s'", containerName)
			err := client.Create(ctx, &ctr)
			require.NoError(t, err, "Could not create Container '%s'", containerName)

			updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

			t.Logf("Start following logs for Container '%s'...", containerName)
			logsErrCh := make(chan error, 1)
			logStreamOpen := concurrency.NewAutoResetEvent(false)
			go func() {
				opts := tc.opts
				opts.Source = "stdout"
				// Run this in a separate goroutine to make sure we open the log stream before we start writing logs.
				logsErrCh <- waitForObjectLogs(ctx, updatedCtr, opts, tc.expectedLines, logStreamOpen)
			}()

			<-logStreamOpen.Wait()

			t.Logf("Simulating logging for Container '%s'...", containerName)
			for _, line := range lines {
				logErr := containerOrchestrator.SimulateContainerLogging(updatedCtr.Status.ContainerID, apiv1.LogStreamSourceStdout, osutil.WithNewline(line))
				require.NoError(t, logErr, "could not simulate logging to stdout")
			}

			err = <-logsErrCh
			require.NoError(t, err, "Could not follow logs for Container '%s'", containerName)
		})
	}
}

// Verify that logs can be captured in follow mode when log stream is opened after the Container has exited.
func TestContainerLogsFollowAfterExit(t *testing.T) {
	const containerName = "test-container-logs-follow-after-exit"
	const imageName = containerName + "-image"

	lines := [][]byte{
		[]byte("Standard output log line 1"),
		[]byte("Standard output log line 2"),
		[]byte("Standard output log line 3"),
	}

	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Simulating logging for Container '%s'...", ctr.ObjectMeta.Name)
	for _, line := range lines {
		logErr := containerOrchestrator.SimulateContainerLogging(updatedCtr.Status.ContainerID, apiv1.LogStreamSourceStdout, osutil.WithNewline(line))
		require.NoError(t, logErr, "could not simulate logging to stdout")
	}

	t.Logf("Transitioning Container '%s' to 'Exited' state...", ctr.ObjectMeta.Name)
	exitErr := containerOrchestrator.SimulateContainerExit(ctx, updatedCtr.Status.ContainerID, 0)
	require.NoError(t, exitErr)
	updatedCtr = ensureContainerState(t, ctx, client, updatedCtr, apiv1.ContainerStateExited)

	t.Logf("Start following logs for Container '%s'...", ctr.ObjectMeta.Name)
	opts := apiv1.LogOptions{
		Follow:     true,
		Source:     "stdout",
		Timestamps: false,
	}
	logsErr := waitForObjectLogs(ctx, updatedCtr, opts, lines, nil)
	require.NoError(t, logsErr, "Could not follow logs for Container '%s'", ctr.ObjectMeta.Name)
}

// Verify that Container startup logs can be captured.
func TestContainerStartupLogs(t *testing.T) {
	const containerName = "test-container-startup-logs"
	const imageName = containerName + "-image"

	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	startupStdoutLines := [][]byte{
		[]byte("Standard output startup log line 1"),
		[]byte("Standard output startup log line 2"),
	}
	startupStderrLines := [][]byte{
		[]byte("Standard error startup log line 1"),
		[]byte("Standard error startup log line 2"),
	}

	containerOrchestrator.SimulateContainerStartupLogs(ctr.Spec.Image,
		bytes.Join(startupStdoutLines, osutil.LineSep()),
		bytes.Join(startupStderrLines, osutil.LineSep()),
	)

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	t.Logf("Ensure startup logs can be captured for Container '%s'...", ctr.ObjectMeta.Name)
	useCases := []apiv1.LogOptions{
		{Follow: false, Source: "startup_stdout", Timestamps: false},
		{Follow: false, Source: "startup_stderr", Timestamps: false},
		{Follow: true, Source: "startup_stdout", Timestamps: false},
		{Follow: true, Source: "startup_stderr", Timestamps: false},
		{Follow: false, Source: "startup_stdout", Timestamps: true},
		{Follow: false, Source: "startup_stderr", Timestamps: true},
		{Follow: true, Source: "startup_stdout", Timestamps: true},
		{Follow: true, Source: "startup_stderr", Timestamps: true},
	}
	for _, opts := range useCases {
		var expected [][]byte
		if opts.Source == "startup_stdout" {
			expected = slices.Map[[]byte, []byte](startupStdoutLines, bytes.Clone)
		} else {
			expected = slices.Map[[]byte, []byte](startupStderrLines, bytes.Clone)
		}
		if opts.Timestamps {
			for i, line := range expected {
				expected[i] = bytes.Join([][]byte{[]byte(osutil.RFC3339MiliTimestampRegex), []byte(" "), line}, nil)
			}
		}
		waitErr := waitForObjectLogs(ctx, updatedCtr, opts, expected, nil)
		require.NoError(t, waitErr, "Could not capture startup logs for Container '%s' (with options %s)", updatedCtr.ObjectMeta.Name, opts.String())
	}
}

// Verify that additional logs are reported in follow mode as soon as they are written.
// This is similar to TestContainerLogsFollowFromStart, but we write logs one line at a time,
// and verify each line separately.
func TestContainerLogsFollowIncremental(t *testing.T) {
	const containerName = "test-container-logs-follow-incremental"
	const imageName = containerName + "-image"

	lines := [][]byte{
		[]byte("Standard output log line 1"),
		[]byte("Standard output log line 2"),
		[]byte("Standard output log line 3"),
	}
	writeLine := concurrency.NewAutoResetEvent(false)
	defer writeLine.SetAndFreeze() // Make sure writer goroutine ends when the test exits, no matter the outcome.

	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	go func() {
		for _, line := range lines {
			writeLine.Wait()
			logErr := containerOrchestrator.SimulateContainerLogging(updatedCtr.Status.ContainerID, apiv1.LogStreamSourceStdout, osutil.WithNewline(line))
			require.NoError(t, logErr, "could not simulate logging to stdout")
		}
	}()

	t.Logf("Start following logs for Container '%s'...", updatedCtr.ObjectMeta.Name)
	opts := apiv1.LogOptions{
		Follow:     true,
		Source:     "stdout",
		Timestamps: false,
	}
	logStream, logStreamErr := openLogStream(ctx, updatedCtr, opts, nil)
	require.NoError(t, logStreamErr, "Could not open log stream for Container '%s'", updatedCtr.ObjectMeta.Name)

	scanner := bufio.NewScanner(usvc_io.NewContextReader(ctx, logStream, true /* leverageReadCloser */))
	for i, line := range lines {
		writeLine.Set()
		gotLine := scanner.Scan()
		require.True(t, gotLine, "Could not read line %d from log stream for Container '%s', the reported error was %v", i, updatedCtr.ObjectMeta.Name, scanner.Err())
		require.Equal(t, string(line), scanner.Text(), "Log line %d does not match expected content for Container '%s'", i, updatedCtr.ObjectMeta.Name)
	}
}

// Verify that logs in follow mode end when Executable is deleted
func TestContainerLogsFollowStreamEndsOnDelete(t *testing.T) {
	const containerName = "test-container-logs-follow-stream-ends-on-delete"
	const imageName = containerName + "-image"

	lines := [][]byte{
		[]byte("Standard output log line 1"),
		[]byte("Standard output log line 2"),
		[]byte("Standard output log line 3"),
	}
	startWriting := concurrency.NewAutoResetEvent(false)
	defer startWriting.SetAndFreeze() // Make sure writer goroutine ends when the test exits, no matter the outcome.

	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containerName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)

	go func() {
		startWriting.Wait()
		for _, line := range lines {
			logErr := containerOrchestrator.SimulateContainerLogging(updatedCtr.Status.ContainerID, apiv1.LogStreamSourceStdout, osutil.WithNewline(line))
			require.NoError(t, logErr, "could not simulate logging to stdout")
		}
	}()

	t.Logf("Start following logs for Container '%s'...", updatedCtr.ObjectMeta.Name)
	opts := apiv1.LogOptions{
		Follow:     true,
		Source:     "stdout",
		Timestamps: false,
	}
	logStream, logStreamErr := openLogStream(ctx, updatedCtr, opts, startWriting)
	require.NoError(t, logStreamErr, "Could not open log stream for Container '%s'", updatedCtr.ObjectMeta.Name)

	scanner := bufio.NewScanner(usvc_io.NewContextReader(ctx, logStream, true /* leverageReadCloser */))
	for i, line := range lines {
		gotLine := scanner.Scan()
		require.True(t, gotLine, "Could not read line %d from log stream for Container '%s', the reported error was %v", i, updatedCtr.ObjectMeta.Name, scanner.Err())
		require.Equal(t, string(line), scanner.Text(), "Log line %d does not match expected content for Container '%s'", i, updatedCtr.ObjectMeta.Name)
	}

	t.Logf("Deleting Container '%s'...", ctr.ObjectMeta.Name)
	err = client.Delete(ctx, updatedCtr.DeepCopy())
	require.NoError(t, err, "Could not delete Container '%s'", updatedCtr.ObjectMeta.Name)

	t.Logf("Ensure log stream ends when Container '%s' is deleted...", updatedCtr.ObjectMeta.Name)
	gotLine := scanner.Scan()
	require.False(t, gotLine, "Unexpectedly read a line from log stream for Container '%s' after it was deleted", updatedCtr.ObjectMeta.Name)
	if scanner.Err() != nil {
		require.ErrorContains(t, scanner.Err(), "response body closed", "The log stream for Container '%s' was not closed as expected")
	}
}

// Ensure that the Container health status changes according to its state (no health probes).
// When running the Container should be Healthy.
// If the container fails to start, it should be Unhealthy.
// Stopped with zero exit code--Caution. Stopped with non-zero exit code--Unhealthy.
func TestContainerHealthBasic(t *testing.T) {
	type testcase struct {
		description            string
		containerName          string
		simulateStartupFailure bool
		exitCode               int32
		expectedState          apiv1.ContainerState
		expectedHealthStatus   apiv1.HealthStatus
	}

	testcases := []testcase{
		{
			description:            "exit-zero",
			containerName:          "container-health-basic-exit-zero",
			simulateStartupFailure: false,
			exitCode:               0,
			expectedState:          apiv1.ContainerStateExited,
			// Only running containers can be healthy
			expectedHealthStatus: apiv1.HealthStatusCaution,
		},
		{
			description:            "exit-non-zero",
			containerName:          "container-health-basic-exit-non-zero",
			simulateStartupFailure: false,
			exitCode:               1,
			expectedState:          apiv1.ContainerStateExited,
			expectedHealthStatus:   apiv1.HealthStatusUnhealthy,
		},
		{
			// Note: this test case will take about 30 seconds to complete because the controller
			// will repeatedly re-try to start the container before finally giving up.
			description:            "startup-failure",
			containerName:          "container-health-basic-startup-failure",
			simulateStartupFailure: true,
			expectedState:          apiv1.ContainerStateFailedToStart,
			expectedHealthStatus:   apiv1.HealthStatusUnhealthy,
		},
	}

	t.Parallel()

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			ctr := apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.containerName,
					Namespace: metav1.NamespaceNone,
				},
				Spec: apiv1.ContainerSpec{
					Image: tc.containerName + "-image",
				},
			}

			if tc.simulateStartupFailure {
				errMsg := fmt.Sprintf("Simulating Container '%s' startup failure", ctr.ObjectMeta.Name)
				containerOrchestrator.FailMatchingContainers(ctx, ctr.ObjectMeta.Name, 1, errMsg)
			}

			t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
			err := client.Create(ctx, &ctr)
			require.NoError(t, err, "Could not create Container object")

			if !tc.simulateStartupFailure {
				updatedCtr, _ := ensureContainerRunning(t, ctx, &ctr)
				require.Equal(t, apiv1.HealthStatusHealthy, updatedCtr.Status.HealthStatus, "Expected the Container to be healthy")

				t.Logf("Simulating Container '%s' exit with zero exit code...", ctr.ObjectMeta.Name)
				err = containerOrchestrator.SimulateContainerExit(ctx, updatedCtr.Status.ContainerID, tc.exitCode)
				require.NoError(t, err, "could not simulate Container exit")
			}

			t.Logf("Ensure Container '%s' state and health status are updated...", ctr.ObjectMeta.Name)
			waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
				hasFinishTimestamp := !c.Status.FinishTimestamp.IsZero()
				inExpectedState := c.Status.State == tc.expectedState
				hasExpectedHealthStatus := c.Status.HealthStatus == tc.expectedHealthStatus
				return hasFinishTimestamp && inExpectedState && hasExpectedHealthStatus, nil
			})
		})
	}
}

// Ensure that Container health status changes when health probe results change (single probe).
func TestContainerHealthSingleProbe(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	t.Logf("Setting up HTTP server for health probe responses...")
	healthEndpoint := internal_testutil.NewTestHttpEndpoint(ctx)

	const testName = "container-health-single-probe"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			HealthProbes: []apiv1.HealthProbe{
				{
					Name: "healthz",
					Type: apiv1.HealthProbeTypeHttp,
					HttpProbe: &apiv1.HttpProbe{
						Url: healthEndpoint.Url(),
					},
					Schedule: apiv1.HealthProbeSchedule{
						Interval:     metav1.Duration{Duration: 500 * time.Millisecond},
						InitialDelay: &metav1.Duration{Duration: 1 * time.Second},
					},
				},
			},
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Ensure Container '%s' is running, but unhealthy...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateRunning && c.Status.HealthStatus == apiv1.HealthStatusUnhealthy, nil
	})

	require.Len(t, updatedCtr.Status.HealthProbeResults, 1, "Expected a single health probe result for Container '%s'", ctr.ObjectMeta.Name)
	unhealthyTimestamp := updatedCtr.Status.HealthProbeResults[0].Timestamp
	require.NotZero(t, unhealthyTimestamp, "Expected a valid timestamp for the unhealthy health probe result for Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Changing health probe response to healthy...")
	time.Sleep(10 * time.Millisecond) // Ensure the timestamp of the next health probe result is different
	healthEndpoint.SetOutcome(apiv1.HealthProbeOutcomeSuccess)

	t.Logf("Ensure Container '%s' health status becomes healthy...", ctr.ObjectMeta.Name)
	updatedCtr = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateRunning && c.Status.HealthStatus == apiv1.HealthStatusHealthy, nil
	})
	require.Len(t, updatedCtr.Status.HealthProbeResults, 1, "Expected a single health probe result for Container '%s'", ctr.ObjectMeta.Name)
	healthyTimestamp := updatedCtr.Status.HealthProbeResults[0].Timestamp
	require.NotZero(t, healthyTimestamp, "Expected a valid timestamp for the healthy health probe result for Container '%s'", ctr.ObjectMeta.Name)
	require.True(t, healthyTimestamp.After(unhealthyTimestamp.Time), "Expected healthy health probe result to be newer than the unhealthy one for Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Changing health probe response back to to unhealthy...")
	healthEndpoint.SetOutcome(apiv1.HealthProbeOutcomeFailure)

	t.Logf("Ensure Container '%s' is running and considered unhealthy again...", ctr.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.HealthStatus == apiv1.HealthStatusUnhealthy, nil
	})

	t.Logf("Deleting Container '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, c *apiv1.Container) error {
		return client.Delete(ctx, c)
	})
	require.NoError(t, err, "Could not delete Container '%s'", ctr.ObjectMeta.Name)
}

// Ensure that Container health status changes when health probe results change (multiple probes).
func TestContainerHealthMultipleProbes(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	t.Logf("Setting up HTTP probe endpoints...")
	he1 := internal_testutil.NewTestHttpEndpoint(ctx)
	he2 := internal_testutil.NewTestHttpEndpoint(ctx)
	he1.SetOutcome(apiv1.HealthProbeOutcomeSuccess)
	he2.SetOutcome(apiv1.HealthProbeOutcomeSuccess)

	const testName = "container-health-multiple-probes"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			HealthProbes: []apiv1.HealthProbe{
				{
					Name: "p1",
					Type: apiv1.HealthProbeTypeHttp,
					HttpProbe: &apiv1.HttpProbe{
						Url: he1.Url(),
					},
					Schedule: apiv1.HealthProbeSchedule{
						Interval: metav1.Duration{Duration: 500 * time.Millisecond},
					},
				},
				{
					Name: "p2",
					Type: apiv1.HealthProbeTypeHttp,
					HttpProbe: &apiv1.HttpProbe{
						Url: he2.Url(),
					},
					Schedule: apiv1.HealthProbeSchedule{
						Interval: metav1.Duration{Duration: 500 * time.Millisecond},
					},
				},
			},
		},
	}

	t.Logf("Creating Container '%s'...", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Ensure Container '%s' is running and healthy...", ctr.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		running := c.Status.State == apiv1.ContainerStateRunning
		healthy := c.Status.HealthStatus == apiv1.HealthStatusHealthy
		hasExpectedResults := healthy &&
			health.VerifyHealthResults(map[string]apiv1.HealthProbeOutcome{
				"p1": apiv1.HealthProbeOutcomeSuccess,
				"p2": apiv1.HealthProbeOutcomeSuccess,
			}, c.Status.HealthProbeResults) == nil
		return running && healthy && hasExpectedResults, nil
	})

	t.Logf("Changing health probe 1 response to unhealthy and verifying the Container '%s' status changes accordingly...", ctr.ObjectMeta.Name)
	he1.SetOutcome(apiv1.HealthProbeOutcomeFailure)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		unhealthy := c.Status.HealthStatus == apiv1.HealthStatusUnhealthy
		hasExpectedResults := unhealthy &&
			health.VerifyHealthResults(map[string]apiv1.HealthProbeOutcome{
				"p1": apiv1.HealthProbeOutcomeFailure,
				"p2": apiv1.HealthProbeOutcomeSuccess,
			}, c.Status.HealthProbeResults) == nil
		return unhealthy && hasExpectedResults, nil
	})

	t.Logf("Changing health probe 2 response to unhealthy and verifying the Container '%s' status changes accordingly...", ctr.ObjectMeta.Name)
	he2.SetOutcome(apiv1.HealthProbeOutcomeFailure)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		unhealthy := c.Status.HealthStatus == apiv1.HealthStatusUnhealthy
		hasExpectedResults := unhealthy &&
			health.VerifyHealthResults(map[string]apiv1.HealthProbeOutcome{
				"p1": apiv1.HealthProbeOutcomeFailure,
				"p2": apiv1.HealthProbeOutcomeFailure,
			}, c.Status.HealthProbeResults) == nil
		return unhealthy && hasExpectedResults, nil
	})

	t.Logf("Changing health probe 1 response back to healthy and verifying the Container '%s' status changes accordingly...", ctr.ObjectMeta.Name)
	he1.SetOutcome(apiv1.HealthProbeOutcomeSuccess)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		unhealthy := c.Status.HealthStatus == apiv1.HealthStatusUnhealthy
		hasExpectedResults := unhealthy &&
			health.VerifyHealthResults(map[string]apiv1.HealthProbeOutcome{
				"p1": apiv1.HealthProbeOutcomeSuccess,
				"p2": apiv1.HealthProbeOutcomeFailure,
			}, c.Status.HealthProbeResults) == nil
		return unhealthy && hasExpectedResults, nil
	})

	t.Logf("Changing health probe 2 response back to healthy and verifying the Container '%s' becomes healthy...", ctr.ObjectMeta.Name)
	he2.SetOutcome(apiv1.HealthProbeOutcomeSuccess)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		healthy := c.Status.HealthStatus == apiv1.HealthStatusHealthy
		hasExpectedResults := healthy &&
			health.VerifyHealthResults(map[string]apiv1.HealthProbeOutcome{
				"p1": apiv1.HealthProbeOutcomeSuccess,
				"p2": apiv1.HealthProbeOutcomeSuccess,
			}, c.Status.HealthProbeResults) == nil
		return healthy && hasExpectedResults, nil
	})

	t.Logf("Container '%s' is healthy and has the expected health probe results", ctr.ObjectMeta.Name)

	t.Logf("Deleting Container '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, c *apiv1.Container) error {
		return client.Delete(ctx, c)
	})
	require.NoError(t, err, "Could not delete Container '%s'", ctr.ObjectMeta.Name)
}

// Ensure that a Container health probe with "until success" schedule works as expected
// (stops after the first successful probe execution).
func TestContainerHealthScheduleUntilSuccess(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	t.Logf("Setting up HTTP server for health probe responses...")
	healthEndpoint := internal_testutil.NewTestHttpEndpoint(ctx)

	const testName = "container-health-schedule-until-success"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			HealthProbes: []apiv1.HealthProbe{
				{
					Name: "healthz",
					Type: apiv1.HealthProbeTypeHttp,
					HttpProbe: &apiv1.HttpProbe{
						Url: healthEndpoint.Url(),
					},
					Schedule: apiv1.HealthProbeSchedule{
						Interval: metav1.Duration{Duration: 100 * time.Millisecond},
						Kind:     apiv1.HealthProbeScheduleUntilSuccess,
					},
				},
			},
		},
	}

	t.Logf("Creating Container '%s'...", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Ensure Container '%s' is running, but considered unhealthy...", ctr.ObjectMeta.Name)
	updatedCtr := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		running := c.Status.State == apiv1.ContainerStateRunning
		unhealthy := c.Status.HealthStatus == apiv1.HealthStatusUnhealthy
		return running && unhealthy, nil
	})
	require.Len(t, updatedCtr.Status.HealthProbeResults, 1, "Expected a single health probe result for Container '%s'", ctr.ObjectMeta.Name)
	unhealthyTimestamp := updatedCtr.Status.HealthProbeResults[0].Timestamp
	require.NotZero(t, unhealthyTimestamp, "Expected a valid timestamp for the unhealthy health probe result for Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Changing health probe response to healthy...")
	time.Sleep(10 * time.Millisecond) // Ensure the timestamp of the next health probe result is different
	healthEndpoint.SetOutcome(apiv1.HealthProbeOutcomeSuccess)

	t.Logf("Ensure Container '%s' is running and considered healthy...", ctr.ObjectMeta.Name)
	updatedCtr = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.HealthStatus == apiv1.HealthStatusHealthy, nil
	})
	require.Len(t, updatedCtr.Status.HealthProbeResults, 1, "Expected a single health probe result for Container '%s'", ctr.ObjectMeta.Name)
	healthyTimestamp := updatedCtr.Status.HealthProbeResults[0].Timestamp
	require.NotZero(t, healthyTimestamp, "Expected a valid timestamp for the healthy health probe result for Container '%s'", ctr.ObjectMeta.Name)
	require.True(t, healthyTimestamp.After(unhealthyTimestamp.Time), "Expected healthy health probe result to be newer than the unhealthy one for Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Changing health probe response back to to unhealthy (this should have NO effect on Container health)...")
	healthEndpoint.SetOutcome(apiv1.HealthProbeOutcomeFailure)
	// Sleep for a while to give the controller chance to execute the probe again (if it would)
	time.Sleep(ctr.Spec.HealthProbes[0].Schedule.Interval.Duration * 5)

	t.Logf("Ensure Container '%s' is still running and considered healthy...", ctr.ObjectMeta.Name)
	finalExe := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.HealthStatus == apiv1.HealthStatusHealthy, nil
	})
	resultDiff, _ := updatedCtr.Status.HealthProbeResults[0].Diff(&finalExe.Status.HealthProbeResults[0])
	require.Equalf(t, apiv1.DiffNone, resultDiff, "Expected the health probe result to remain the same for Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Deleting Container '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, c *apiv1.Container) error {
		return client.Delete(ctx, c)
	})
	require.NoError(t, err, "Could not delete Container '%s'", ctr.ObjectMeta.Name)
}

// Ensure the Container attached to specific network is cleaned up properly (including ContainerNetworkConnection)
// even if the container fails to start.
// This is an important .NET Aspire use case (which creates a separate network for every run).
func TestContainerNetworkConnectedFailedStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-netconn-startfailed"
	const imageName = testName + "-image"
	const networkName = testName + "-network"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkName,
			Namespace: metav1.NamespaceNone,
		},
	}

	errMsg := fmt.Sprintf("Simulation Container '%s' startup failure...", testName)
	containerOrchestrator.FailMatchingContainers(ctx, testName, 2, errMsg)

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNetwork := ensureNetworkCreated(t, ctx, &net)

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name: networkName,
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	// This will take a while--the controller will try to start the container multiple times before giving up.
	t.Logf("Ensure Container '%s' state is 'failed to start'...", ctr.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		statusUpdated := c.Status.State == apiv1.ContainerStateFailedToStart
		messageOK := strings.Contains(c.Status.Message, errMsg)
		unhealthy := c.Status.HealthStatus == apiv1.HealthStatusUnhealthy
		return statusUpdated && messageOK && unhealthy, nil
	})

	// Retrieve the Container object to get the ContainerID.
	var updatedCtr apiv1.Container
	err = client.Get(ctx, ctrl_client.ObjectKeyFromObject(&ctr), &updatedCtr)
	require.NoError(t, err, "could not get updated Container object for container '%s'", ctr.ObjectMeta.Name)
	require.NotEmptyf(t, updatedCtr.Status.ContainerID, "expected ContainerID to be set for container '%s'", ctr.ObjectMeta.Name)

	// Ensure that the Container is not connected to the network and that there are no ContainerNetworkConnection objects left for it.
	t.Logf("Ensure Container '%s' is disconnected from ContainerNetwork '%s'...", ctr.ObjectMeta.Name, net.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(updatedNetwork), func(currentNet *apiv1.ContainerNetwork) (bool, error) {
		return slices.All(currentNet.Status.ContainerIDs, func(id string) bool {
			return string(updatedCtr.Status.ContainerID) != id
		}), nil
	})

	ctrl_testutil.WaitObjectCount[apiv1.ContainerNetworkConnection, apiv1.ContainerNetworkConnectionList](
		t, ctx, client, 0,
		fmt.Sprintf("ensure that there are no ContainerNetworkConnection objects left for container '%s'", ctr.ObjectMeta.Name),
		func(cnc *apiv1.ContainerNetworkConnection) bool {
			return cnc.Spec.ContainerID == updatedCtr.Status.ContainerID
		},
	)
}

func TestContainerCreateFilesDefaultValues(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-create-files-default-values"
	const imageName = testName + "-image"

	testStart := time.Now()

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			CreateFiles: []apiv1.CreateFileSystem{
				{
					Destination: "/tmp",
					Entries: []apiv1.FileSystemEntry{
						{
							Name:     "hello.txt",
							Contents: "hello!",
						},
					},
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container object")

	_, inspected := ensureContainerRunning(t, ctx, &ctr)
	files, getFileErr := containerOrchestrator.GetCreatedFiles(inspected.Id)
	require.NoError(t, getFileErr, "could not get created files")

	require.Len(t, files, 1, "expected to find a single copied file")
	require.Equal(t, ctr.Spec.CreateFiles[0].Destination, files[0].Destination, "copied file destination does not match")
	require.LessOrEqual(t, testStart, files[0].ModTime, "copied file mod time is not greater than or equal to the test start time")
	require.Equal(t, osutil.DefaultUmaskBitmask, files[0].Umask, "copied file mode does not match expected default value")
	require.Equal(t, int32(0), files[0].DefaultOwner, "copied file owner id does not match expected default value")
	require.Equal(t, int32(0), files[0].DefaultGroup, "copied file group id does not match expected default value")

	items, itemsErr := files[0].GetTarItems()
	require.NoError(t, itemsErr, "could not get tar items")
	require.Len(t, items, 1, "expected a single tar recrd")

	require.Equal(t, 0, items[0].Uid, "copied file item owner id does not match expected default value")
	require.Equal(t, 0, items[0].Gid, "copied file item group id does not match expected default value")
	require.Equal(t, int64(osutil.PermissionOwnerReadWriteOthersRead), items[0].Mode, "copied file item mode does not match expected default value")
}

func TestContainerCreateFilesMultipleFiles(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-create-files-multiple-files"
	const imageName = testName + "-image"

	testStart := time.Now()

	itemOwner := int32(0)
	umask := fs.FileMode(077)
	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			CreateFiles: []apiv1.CreateFileSystem{
				{
					Destination: "/tmp",
					Entries: []apiv1.FileSystemEntry{
						{
							Name: "touch.txt",
						},
					},
				},
				{
					Destination:  "/some/path",
					Umask:        &umask,
					DefaultOwner: 1000,
					DefaultGroup: 1000,
					Entries: []apiv1.FileSystemEntry{
						{
							Type: apiv1.FileSystemEntryTypeDir,
							Name: "some-dir",
							Entries: []apiv1.FileSystemEntry{
								{
									Name:  "hello.txt",
									Owner: &itemOwner,
								},
							},
						},
					},
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "could not create a Container object")

	_, inspected := ensureContainerRunning(t, ctx, &ctr)
	files, getFileErr := containerOrchestrator.GetCreatedFiles(inspected.Id)
	require.NoError(t, getFileErr, "could not get copied files")

	require.Len(t, files, 2, "expected to find a single copied file")
	require.Equal(t, ctr.Spec.CreateFiles[0].Destination, files[0].Destination, "copied file destination does not match")
	require.LessOrEqual(t, testStart, files[0].ModTime, "copied file mod time is not greater than or equal to the test start time")
	require.Equal(t, fs.FileMode(osutil.DefaultUmaskBitmask), files[0].Umask, "copied file umask does not match expected default value")
	require.Equal(t, int32(0), files[0].DefaultOwner, "copied file owner id does not match expected default value")
	require.Equal(t, int32(0), files[0].DefaultGroup, "copied file group id does not match expected default value")

	require.Equal(t, ctr.Spec.CreateFiles[1].Destination, files[1].Destination, "copied file destination does not match")
	require.LessOrEqual(t, testStart, files[1].ModTime, "copied file mod time is not greater than or equal to the test start time")
	require.Equal(t, umask, files[1].Umask, "copied file umask does not match expected value")

	items, itemsErr := files[1].GetTarItems()
	require.NoError(t, itemsErr, "could not get tar items")
	require.Len(t, items, 2, "expected two tar records")

	require.Equal(t, "/some/path/some-dir", items[0].Name, "copied file item name does not match expected value")
	require.Equal(t, 1000, items[0].Uid, "copied file item owner id does not match expected value")
	require.Equal(t, 1000, items[0].Gid, "copied file item group id does not match expected value")
	require.Equal(t, int64(osutil.PermissionOnlyOwnerReadWriteTraverse|fs.ModeDir), items[0].Mode, "copied file item mode does not match expected value")

	require.Equal(t, "/some/path/some-dir/hello.txt", items[1].Name, "copied file item name does not match expected value")
	require.Equal(t, 0, items[1].Uid, "copied file item owner id does not match expected value")
	require.Equal(t, 1000, items[1].Gid, "copied file item group id does not match expected value")
	require.Equal(t, int64(osutil.PermissionOnlyOwnerReadWrite), items[1].Mode, "copied file item mode does not match expected value")
}

// Even if Docker container stops very quickly, the state of corresponding Container should be "Exited"
// (and not something else, in particular not "FailedToStart").
func TestContainerFastExiting(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-fast-exiting"
	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
		},
	}

	ctrEvtCh := concurrency.NewUnboundedChan[containers.EventMessage](ctx)
	sub, subErr := containerOrchestrator.WatchContainers(ctrEvtCh.In)
	require.NoError(t, subErr, "could not subscribe to container events")
	defer sub.Cancel()

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Simulating fast exit of Container '%s'...", ctr.ObjectMeta.Name)
ContainerEventsLoop:
	for {
		select {
		case msg := <-ctrEvtCh.Out:
			containerCreated := msg.Action == containers.EventActionStart && msg.Source == containers.EventSourceContainer &&
				maps.HasExactValue(msg.Attributes, ctrl_testutil.ContainerNameAttribute, ctr.Spec.ContainerName)
			if containerCreated {
				sub.Cancel()
				ceErr := containerOrchestrator.SimulateContainerExit(ctx, ctr.Spec.ContainerName, 0)
				require.NoError(t, ceErr, "could not simulate Container exit")
				break ContainerEventsLoop
			}
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for container '%s' to be created", ctr.Spec.ContainerName)
		}
	}

	t.Logf("Ensure Container '%s' state is 'Exited'...", ctr.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited && c.Status.HealthStatus == apiv1.HealthStatusCaution, nil
	})
}

// Similar to TestContainerFastExiting, but the container is also attached to custom network,
// which exercises different code path in the controller.
func TestContainerFastExitingWithNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "container-with-network-fast-exiting"
	const imageName = testName + "-image"
	const networkName = testName + "-network"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:         imageName,
			ContainerName: testName,
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name: networkName,
				},
			},
		},
	}

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	_ = ensureNetworkCreated(t, ctx, &net)

	ctrEvtCh := concurrency.NewUnboundedChan[containers.EventMessage](ctx)
	sub, subErr := containerOrchestrator.WatchContainers(ctrEvtCh.In)
	require.NoError(t, subErr, "could not subscribe to container events")
	defer sub.Cancel()

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Simulating fast exit of Container '%s'...", ctr.ObjectMeta.Name)
ContainerEventsLoop:
	for {
		select {
		case msg := <-ctrEvtCh.Out:
			containerCreated := msg.Action == containers.EventActionStart && msg.Source == containers.EventSourceContainer &&
				maps.HasExactValue(msg.Attributes, ctrl_testutil.ContainerNameAttribute, ctr.Spec.ContainerName)
			if containerCreated {
				sub.Cancel()
				ceErr := containerOrchestrator.SimulateContainerExit(ctx, ctr.Spec.ContainerName, 0)
				require.NoError(t, ceErr, "could not simulate Container exit")
				break ContainerEventsLoop
			}
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for container '%s' to be created", ctr.Spec.ContainerName)
		}
	}

	t.Logf("Ensure Container '%s' state is 'Exited'...", ctr.ObjectMeta.Name)
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited && c.Status.HealthStatus == apiv1.HealthStatusCaution, nil
	})
}

// Verify that Service port and address are injected into a HTTP probe URL and headers for a Container,
// and that the probe is executed correctly.
func TestContainerHttpHealthProbePortInjected(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const baseName = "container-http-health-probe-port-injected"
	const dcpTestDataHeader = "DCP-TEST-DATA"

	t.Logf("Setting up HTTP endpoint for health probe responses...")
	healthEndpoint := internal_testutil.NewTestHttpEndpoint(ctx)
	healthEndpoint.SetOutcome(apiv1.HealthProbeOutcomeSuccess)
	healthEndpointAddr, healthEndpointPort, addrAndPortErr := healthEndpoint.AddressAndPort()
	require.NoError(t, addrAndPortErr, "Could not get address and port for health endpoint")

	probeCalled := make(chan struct{})
	closeOnce := sync.OnceFunc(func() { close(probeCalled) })

	healthEndpoint.SetHealthyResponseObserver(func(r *http.Request) {
		headerValue := r.Header.Get(dcpTestDataHeader)
		require.NotEmpty(t, headerValue, "Expected header '%s' not found in health probe request", dcpTestDataHeader)
		require.Equal(t, fmt.Sprintf("%s:%d", healthEndpointAddr, healthEndpointPort), headerValue)
		closeOnce()
	})

	svc := apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      baseName + "-service",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
			Address:  healthEndpointAddr,
			Port:     healthEndpointPort,
		},
	}

	t.Logf("Creating Service '%s'", svc.ObjectMeta.Name)
	err := client.Create(ctx, &svc)
	require.NoError(t, err, "Could not create Service '%s'", svc.ObjectMeta.Name)

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      baseName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: baseName + "-image",
			HealthProbes: []apiv1.HealthProbe{
				{
					Name: "healthz",
					Type: apiv1.HealthProbeTypeHttp,
					HttpProbe: &apiv1.HttpProbe{
						Url: fmt.Sprintf(`http://{{- addressFor "%s" -}}:{{- portFor "%s" -}}%s`, svc.Name, svc.Name, internal_testutil.TestHttpEndpointPath),
						Headers: []apiv1.HttpHeader{
							{
								Name:  dcpTestDataHeader,
								Value: fmt.Sprintf(`{{- addressFor "%s" -}}:{{- portFor "%s" -}}`, svc.Name, svc.Name),
							},
						},
					},
					Schedule: apiv1.HealthProbeSchedule{
						Interval: metav1.Duration{Duration: 500 * time.Millisecond},
					},
				},
			},
		},
	}

	t.Logf("Creating Container '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create Container '%s'", ctr.ObjectMeta.Name)

	t.Logf("Waiting for the health probe for '%s' to be called...", ctr.ObjectMeta.Name)
	select {
	case <-probeCalled:
		t.Logf("Health probe for '%s' was called successfully", ctr.ObjectMeta.Name)
	case <-ctx.Done():
		t.Fatalf("Health probe for '%s' was not called in time", ctr.ObjectMeta.Name)
	}

	t.Logf("Ensure Container '%s' is running and considered healthy...", ctr.ObjectMeta.Name)
	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(c *apiv1.Container) (bool, error) {
		running := c.Status.State == apiv1.ContainerStateRunning
		healthy := c.Status.HealthStatus == apiv1.HealthStatusHealthy
		return running && healthy, nil
	})

	t.Logf("Deleting Container '%s'...", ctr.ObjectMeta.Name)
	err = retryOnConflict(ctx, ctr.NamespacedName(), func(ctx context.Context, c *apiv1.Container) error {
		return client.Delete(ctx, c)
	})
	require.NoError(t, err, "Could not delete Container '%s'", ctr.ObjectMeta.Name)
}
