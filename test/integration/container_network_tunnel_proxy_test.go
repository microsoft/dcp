package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

// Verifies that ContainerNetworkTunnelProxy can be created and deleted (including finalizer handling).
func TestTunnelProxyCreateDelete(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-tunnel-proxy-create-delete"

	// First create a ContainerNetwork that the tunnel proxy will reference
	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	// Create the ContainerNetworkTunnelProxy object
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:       "test-tunnel",
					ServerPort: 8080,
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	// Wait for the controller to add the finalizer
	t.Log("Waiting for controller to add finalizer to ContainerNetworkTunnelProxy..")
	tunnelProxyFinalizerName := fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)
	_ = waitObjectAssumesState(t, ctx, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return slices.Contains(tp.ObjectMeta.Finalizers, tunnelProxyFinalizerName), nil
	})

	// Now try to delete the object
	t.Logf("Deleting ContainerNetworkTunnelProxy object '%s'", tunnelProxy.ObjectMeta.Name)
	err = retryOnConflict(ctx, tunnelProxy.NamespacedName(), func(ctx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		return client.Delete(ctx, tp)
	})
	require.NoError(t, err, "Could not delete ContainerNetworkTunnelProxy object")

	// Verify the object is really gone from the API server
	t.Log("Waiting for controller to remove finalizer and complete deletion...")
	ctrlutil.WaitObjectDeleted(t, ctx, client, &tunnelProxy)
}

// Verifies that ContainerNetworkTunnelProxy can be created without an existing ContainerNetwork and transitions to Running state once the network is created.
func TestTunnelProxyDelayedNetworkCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-tunnel-proxy-delayed-network-creation"

	// Create the ContainerNetworkTunnelProxy object WITHOUT creating the ContainerNetwork first
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: testName + "-network",
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:       "test-tunnel",
					ServerPort: 8080,
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s' without referenced ContainerNetwork", tunnelProxy.ObjectMeta.Name)
	err := client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	// Wait for the controller to do initial pass over the object
	t.Log("Waiting for controller to add finalizer to ContainerNetworkTunnelProxy...")
	_ = waitObjectAssumesState(t, ctx, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStatePending, nil
	})

	// Now create the ContainerNetwork that the tunnel proxy references
	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tunnelProxy.Spec.ContainerNetworkName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	createNetworkErr := client.Create(ctx, &network)
	require.NoError(t, createNetworkErr, "Could not create a ContainerNetwork object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	_ = waitObjectAssumesState(t, ctx, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
}
