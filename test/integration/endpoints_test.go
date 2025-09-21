package integration_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestEndpointCreatedAndDeletedForExecutable(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-endpoint-creation-executable"
	const serviceName = testName + "-svc"

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":5001}]`, serviceName),
			},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "/path/to/" + testName,
		},
	}

	t.Logf("Creating Executable '%s'", exe.ObjectMeta.Name)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable")

	endpoint := waitEndpointExists(t, ctx, "check if Endpoint created", func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == serviceName &&
			e.Spec.Address == networking.IPv4LocalhostDefaultAddress &&
			e.Spec.Port == 5001, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Logf("Deleting Executable '%s'...", exe.ObjectMeta.Name)
	err = retryOnConflict(ctx, exe.NamespacedName(), func(ctx context.Context, currentExe *apiv1.Executable) error {
		return client.Delete(ctx, currentExe)
	})
	require.NoError(t, err, "Could not delete Executable")

	t.Log("Check if Endpoint deleted...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, endpoint)
	t.Log("Endpoint deleted")
}

func TestEndpointCreatedAndDeletedForContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "endpoint-creation-deletion-container"
	const svcName = testName + "-svc"

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: fmt.Sprintf(`[{"serviceName":"%s","port":80}]`, svcName),
			},
		},
		Spec: apiv1.ContainerSpec{
			Image: testName + "-image",
			Ports: []apiv1.ContainerPort{
				{
					ContainerPort: 80,
					HostPort:      8080,
				},
			},
		},
	}

	t.Logf("Creating Container '%s'", container.ObjectMeta.Name)
	err := client.Create(ctx, &container)
	require.NoError(t, err, "Could not create a Container")

	updatedCtr, _ := ensureContainerRunning(t, ctx, &container)

	waitEndpointExists(t, ctx, "check if Endpoint created", func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == svcName &&
			e.Spec.Address == networking.IPv4LocalhostDefaultAddress &&
			e.Spec.Port == 8080, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Deleting Container...")
	err = retryOnConflict(ctx, container.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		return client.Delete(ctx, currentCtr)
	})
	require.NoError(t, err, "Could not delete Container")

	t.Logf("Ensure that Container object really disappeared from the API server, '%s'...", container.ObjectMeta.Name)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &container)

	inspected, err := containerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedCtr.Status.ContainerID},
	})
	require.Error(t, err, "expected the container to be gone")
	require.Len(t, inspected, 0, "expected the container to be gone")

	t.Log("Check if Endpoint deleted...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &container)
	t.Log("Endpoint deleted")
}

func TestEndpointDeletedIfExecutableStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-endpoint-deleted-if-executable-stopped"
	const svcName = testName + "-svc"

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":5001}]`, svcName),
			},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: fmt.Sprintf("path/to/%s", testName),
		},
	}

	t.Logf("Creating Executable '%s'", exe.ObjectMeta.Name)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create the Executable")

	t.Log("Check if the Executable is running...")
	pid, err := ensureProcessRunning(ctx, exe.Spec.ExecutablePath)
	require.NoError(t, err, "Executable was not started as expected")

	endpoint := waitEndpointExists(t, ctx, "checking if Endpoint created", func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == svcName && e.Spec.Address == networking.IPv4LocalhostDefaultAddress && e.Spec.Port == 5001, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Simulate Executable stopping...")
	testProcessExecutor.SimulateProcessExit(t, pid, 0)

	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Done(), nil
	})

	t.Log("Check if corresponding Endpoint deleted...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, endpoint)
}

func TestEndpointDeletedIfContainerStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-endpoint-deleted-if-container-stopped"
	const svcName = testName + "-svc"

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: fmt.Sprintf(`[{"serviceName":"%s","port":80}]`, svcName),
			},
		},
		Spec: apiv1.ContainerSpec{
			Image: testName + "-image",
			Ports: []apiv1.ContainerPort{
				{
					ContainerPort: 80,
					HostPort:      8080,
				},
			},
		},
	}

	t.Logf("Creating Container '%s'", container.ObjectMeta.Name)
	err := client.Create(ctx, &container)
	require.NoError(t, err, "Could not create the Container")

	_, inspected := ensureContainerRunning(t, ctx, &container)

	endpoint := waitEndpointExists(t, ctx, "check if Endpoint created", func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == svcName && e.Spec.Address == networking.IPv4LocalhostDefaultAddress && e.Spec.Port == 8080, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Simulate Container stopping...")
	err = containerOrchestrator.SimulateContainerExit(ctx, inspected.Id, 0)
	require.NoError(t, err, "Could not simulate container exit")

	t.Log("Ensure Container object status reflects the state of the running container...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&container), func(c *apiv1.Container) (bool, error) {
		return !c.Status.FinishTimestamp.IsZero() && c.Status.State == apiv1.ContainerStateExited, nil
	})

	t.Log("Check if corresponding Endpoint deleted...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, endpoint)
}

func TestEndpointRecreatedIfContainerPortsChange(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-endpoint-recreated-if-container-ports-change"
	const svcName = testName + "-svc"
	const containerPort = 37229

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: fmt.Sprintf(`[{"serviceName":"%s","port":%d}]`, svcName, containerPort),
			},
		},
		Spec: apiv1.ContainerSpec{
			Image: testName + "-image",
			Ports: []apiv1.ContainerPort{
				{
					ContainerPort: containerPort,
					// No host port specified (host port is dynamically assigned)
				},
			},
		},
	}

	t.Logf("Creating Container '%s'...", container.ObjectMeta.Name)
	err := client.Create(ctx, &container)
	require.NoError(t, err, "Could not create the Container")

	_, inspected := ensureContainerRunning(t, ctx, &container)

	t.Log("Check if related service Endpoint was properly created...")
	verifyContainerEndpoint(t, ctx, inspected, svcName, containerPort)

	t.Logf("Simulating restart of the container '%s'...", inspected.Id)
	restartErr := containerOrchestrator.SimulateContainerRestart(ctx, inspected.Id)
	require.NoError(t, restartErr, "Could not simulate container restart")

	// Inspect the container again to get the new host port (as part of inspected container data)
	_, inspected = ensureContainerRunning(t, ctx, &container)

	t.Logf("Waiting for service Endpoint to be updated...")
	verifyContainerEndpoint(t, ctx, inspected, svcName, containerPort)

	t.Logf("Verify that the old Endpoint was deleted...")
	waitEndpointCount(t, ctx, svcName, 1)
}

func verifyContainerEndpoint(
	t *testing.T,
	ctx context.Context,
	inspected containers.InspectedContainer,
	serviceName string,
	containerPort int,
) {
	_ = waitEndpointExists(t, ctx, "verifying container endpoint", func(e *apiv1.Endpoint) (bool, error) {
		portConfig, found := inspected.Ports[fmt.Sprintf("%d/tcp", containerPort)]
		if !found || len(portConfig) == 0 {
			return false, fmt.Errorf("expected to find port %d/tcp in container inspection result", containerPort)
		}

		hostPort, portParseErr := strconv.Atoi(portConfig[0].HostPort)
		if portParseErr != nil {
			return false, fmt.Errorf("expected to find valid host port in container inspection result: %w", portParseErr)
		}

		return e.Spec.ServiceName == serviceName &&
			e.Spec.Address == networking.IPv4LocalhostDefaultAddress &&
			e.Spec.Port == int32(hostPort), nil
	})
}

func waitEndpointCount(t *testing.T, ctx context.Context, serviceName string, expectedCount int) {
	waitEndpointCountEx(t, ctx, client, serviceName, expectedCount)
}

func waitEndpointCountEx(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, serviceName string, expectedCount int) {
	ctrl_testutil.WaitObjectCount[apiv1.Endpoint, apiv1.EndpointList](t, ctx, apiClient, expectedCount,
		fmt.Sprintf("counting endpoints for service '%s'", serviceName), func(e *apiv1.Endpoint) bool {
			return e.Spec.ServiceName == serviceName
		})
}

func waitEndpointExists(t *testing.T, ctx context.Context, contextStr string, selector func(e *apiv1.Endpoint) (bool, error)) *apiv1.Endpoint {
	return waitEndpointExistsEx(t, ctx, client, contextStr, selector)
}

func waitEndpointExistsEx(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, contextStr string, selector func(e *apiv1.Endpoint) (bool, error)) *apiv1.Endpoint {
	return ctrl_testutil.WaitObjectExists[apiv1.Endpoint, apiv1.EndpointList](t, ctx, apiClient, contextStr, selector)
}
