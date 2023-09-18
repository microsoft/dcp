package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
)

func TestEndpointCreatedAndDeletedForExecutable(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-endpoint-creation-executable",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": `[{"serviceName":"MyExeApp","address":"127.0.0.1","port":5001}]`},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/test-endpoint-creation-executable",
		},
	}

	t.Logf("Creating Executable '%s'", exe.ObjectMeta.Name)
	err := client.Create(ctx, &exe)
	require.NoError(t, err, "Could not create an Executable")

	t.Log("Check if Endpoint created...")
	endpoint := waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == "MyExeApp" &&
			e.Spec.Address == "127.0.0.1" &&
			e.Spec.Port == 5001, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Deleting Executable...")
	err = client.Delete(ctx, &exe)
	require.NoError(t, err, "Could not delete Executable")

	t.Log("Check if Endpoint deleted...")
	waitObjectDeleted[apiv1.Endpoint](t, ctx, ctrl_client.ObjectKeyFromObject(endpoint))
	t.Log("Endpoint deleted")
}

func TestEndpointCreatedAndDeletedForContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "endpoint-creation-deletion"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-endpoint-creation-container",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": `[{"serviceName":"MyContainerApp","port":80}]`},
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

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, container.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")

	t.Log("Check if Endpoint created...")
	waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == "MyContainerApp" &&
			e.Spec.Address == "127.0.0.1" &&
			e.Spec.Port == 8080, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Deleting Container...")
	err = client.Delete(ctx, &container)
	require.NoError(t, err, "Could not delete Container")

	t.Log("Check if corresponding container was deleted...")
	err = ensureContainerDeleted(t, ctx, containerID)
	require.NoError(t, err, "Container was not deleted as expected")

	t.Log("Check if Endpoint deleted...")
	waitObjectDeleted[apiv1.Endpoint](t, ctx, ctrl_client.ObjectKeyFromObject(&container))
	t.Log("Endpoint deleted")
}

func TestEndpointDeletedIfExecutableStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-endpoint-deleted-if-executable-stopped"

	exe := apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				"service-producer": fmt.Sprintf(`[{"serviceName":"%s","address":"127.0.0.1","port":5001}]`, testName),
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

	t.Log("Check if Endpoint created...")
	endpoint := waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == testName && e.Spec.Address == "127.0.0.1" && e.Spec.Port == 5001, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Simulate Executable stopping...")
	processExecutor.SimulateProcessExit(t, pid, 0)

	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Done(), nil
	})

	t.Log("Check if corresponding Endpoint deleted...")
	waitObjectDeleted[apiv1.Endpoint](t, ctx, ctrl_client.ObjectKeyFromObject(endpoint))
}

func TestEndpointDeletedIfContainerStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-endpoint-deleted-if-container-stopped"
	containerID := testName + "-" + testutil.GetRandLetters(t, 6)

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				"service-producer": fmt.Sprintf(`[{"serviceName":"%s","port":80}]`, testName),
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

	t.Log("Check if corresponding container has started...")
	creationTime := time.Now().UTC()
	err = ensureContainerRunning(t, ctx, container.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Container was not started as expected")

	t.Log("Check if Endpoint created...")
	endpoint := waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == testName && e.Spec.Address == "127.0.0.1" && e.Spec.Port == 8080, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Simulate Container stopping...")
	err = simulateContainerExit(t, ctx, container.Spec.Image, containerID, creationTime)
	require.NoError(t, err, "Could not simulate container exit")

	t.Log("Ensure Container object status reflects the state of the running container...")
	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&container), func(c *apiv1.Container) (bool, error) {
		return !c.Status.FinishTimestamp.IsZero() && c.Status.State == apiv1.ContainerStateExited, nil
	})

	t.Log("Check if corresponding Endpoint deleted...")
	waitObjectDeleted[apiv1.Endpoint](t, ctx, ctrl_client.ObjectKeyFromObject(endpoint))
}

func waitEndpointExists(t *testing.T, ctx context.Context, selector func(*apiv1.Endpoint) (bool, error)) *apiv1.Endpoint {
	var updatedObject *apiv1.Endpoint = new(apiv1.Endpoint)

	existsWithExpectedState := func(ctx context.Context) (bool, error) {
		var endpointList apiv1.EndpointList
		err := client.List(ctx, &endpointList)
		if err != nil {
			t.Fatal("unable to list Endpoints from API server", err)
			return false, err
		}

		for _, endpoint := range endpointList.Items {
			if matches, err := selector(&endpoint); err != nil {
				t.Fatal("unable to select Endpoint", err)
				return false, err
			} else if matches {
				updatedObject = &endpoint
				return true, nil
			}
		}

		return false, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, existsWithExpectedState)
	if err != nil {
		t.Fatal("Waiting for Endpoint to exist with desired state failed", err)
		return nil // make the compiler happy
	} else {
		return updatedObject
	}
}
