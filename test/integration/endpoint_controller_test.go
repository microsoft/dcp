package integration_test

import (
	"context"
	"testing"

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
			Name:        "test-endpoint-creation",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": `{"serviceName":"MyExeApp","address":"127.0.0.1","port":5001}`},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "path/to/executable-starts-process",
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
	err = client.Delete(ctx, &exe, ctrl_client.PropagationPolicy(metav1.DeletePropagationForeground))
	require.NoError(t, err, "Could not delete Executable")

	t.Log("Check if Endpoint deleted...")
	waitObjectDeleted[apiv1.Endpoint](t, ctx, ctrl_client.ObjectKeyFromObject(endpoint))
	t.Log("Endpoint deleted")
}

func TestEndpointCreatedAndDeletedForContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	container := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-endpoint-creation",
			Namespace:   metav1.NamespaceNone,
			Annotations: map[string]string{"service-producer": `{"serviceName":"MyContainerApp","address":"127.0.0.1","port":5002}`},
		},
		Spec: apiv1.ContainerSpec{
			Image: "my-container-image",
		},
	}

	t.Logf("Creating Container '%s'", container.ObjectMeta.Name)
	err := client.Create(ctx, &container)
	require.NoError(t, err, "Could not create a Container")

	t.Log("Check if Endpoint created...")
	waitEndpointExists(t, ctx, func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == "MyContainerApp" &&
			e.Spec.Address == "127.0.0.1" &&
			e.Spec.Port == 5002, nil
	})
	t.Log("Found Endpoint with correct spec")

	t.Log("Deleting Container...")
	err = client.Delete(ctx, &container, ctrl_client.PropagationPolicy(metav1.DeletePropagationForeground))
	require.NoError(t, err, "Could not delete Container")

	t.Log("Check if Endpoint deleted...")
	waitObjectDeleted[apiv1.Endpoint](t, ctx, ctrl_client.ObjectKeyFromObject(&container))
	t.Log("Endpoint deleted")
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
