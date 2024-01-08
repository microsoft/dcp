package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCreateNetworkInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-create-network-instance"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	_ = ensureNetworkCreated(t, ctx, &net)
}

func TestRemoveNetworkInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-remove-network-instance"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNet := ensureNetworkCreated(t, ctx, &net)

	t.Logf("Deleting ContainerNetwork object '%s'", net.ObjectMeta.Name)

	err = client.Delete(ctx, updatedNet)
	require.NoError(t, err, "could not delete a ContainerNetwork object")

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(_ context.Context) (bool, error) {
		inspectedNetworks, err := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{updatedNet.Status.ID}})
		if !errors.Is(err, containers.ErrNotFound) || len(inspectedNetworks) != 0 {
			return false, nil
		}

		return true, nil
	})
	require.NoError(t, err, "network was not removed")
}

func ensureNetworkCreated(t *testing.T, ctx context.Context, network *apiv1.ContainerNetwork) *apiv1.ContainerNetwork {
	updatedNet := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(network), func(currentNet *apiv1.ContainerNetwork) (bool, error) {
		if currentNet.Status.State == apiv1.ContainerNetworkStateFailedToStart {
			return false, fmt.Errorf("network creation failed: %s", currentNet.Status.Message)
		}

		return currentNet.Status.ID != "", nil
	})

	inspectedNetworks, err := orchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{updatedNet.Status.ID}})
	require.NoError(t, err, "could not inspect the network")
	require.Len(t, inspectedNetworks, 1, "expected to find a single network")

	return updatedNet
}
