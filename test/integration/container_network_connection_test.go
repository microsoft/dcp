// Copyright (c) Microsoft Corporation. All rights reserved.

package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

func TestContainerNetworkConnectsToExisting(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-container-connects-existing-network"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNetwork := ensureNetworkCreated(t, ctx, &net)

	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name: testName,
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspectedContainer := ensureContainerRunning(t, ctx, &ctr)

	found := slices.ContainsFunc(inspectedContainer.Networks, func(n containers.InspectedContainerNetwork) bool {
		return n.Name == updatedNetwork.Status.NetworkName
	})
	require.True(t, found)
}

func TestContainerNetworkChanges(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-container-connected-network-changes"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNetwork := ensureNetworkCreated(t, ctx, &net)

	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name: testName,
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	updatedCtr, inspectedContainer := ensureContainerRunning(t, ctx, &ctr)

	found := slices.ContainsFunc(inspectedContainer.Networks, func(n containers.InspectedContainerNetwork) bool {
		return n.Name == updatedNetwork.Status.NetworkName
	})
	require.True(t, found)

	net2 := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-2",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net2.ObjectMeta.Name)
	err = client.Create(ctx, &net2)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNetwork2 := ensureNetworkCreated(t, ctx, &net2)

	err = retryOnConflict(ctx, updatedCtr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		updatedNetworks := append(*(currentCtr.Spec.Networks), apiv1.ContainerNetworkConnectionConfig{
			Name: net2.NamespacedName().String(),
		})
		containerPatch.Spec.Networks = &updatedNetworks
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		t.Fatalf("Unable to update Container '%s' to use additional network: %v", updatedCtr.NamespacedName().String(), err)
	}

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		if slices.ContainsFunc(currentCtr.Status.Networks, func(n string) bool {
			return updatedNetwork2.NamespacedName().String() == n
		}) {
			return true, nil
		}

		return false, nil
	})

	err = retryOnConflict(ctx, updatedCtr.NamespacedName(), func(ctx context.Context, currentCtr *apiv1.Container) error {
		containerPatch := currentCtr.DeepCopy()
		// Reset the networks to the original
		containerPatch.Spec.Networks = ctr.Spec.Networks
		return client.Patch(ctx, containerPatch, ctrl_client.MergeFromWithOptions(currentCtr, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		t.Fatalf("Unable to update Container '%s' to use the original network only: %v", updatedCtr.NamespacedName().String(), err)
	}

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(&ctr), func(currentCtr *apiv1.Container) (bool, error) {
		if slices.ContainsFunc(currentCtr.Status.Networks, func(n string) bool {
			return updatedNetwork2.NamespacedName().String() == n
		}) {
			return false, nil
		}

		return true, nil
	})
}

func TestContainerNetworkDoesNotStartUntilNetworkExists(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-container-does-not-start-until-network"

	const imageName = testName + "-image"

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name: testName,
				},
			},
		},
	}

	t.Logf("Creating Container object '%s'", ctr.ObjectMeta.Name)
	err := client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_ = ensureContainerState(t, ctx, client, &ctr, apiv1.ContainerStateStarting)

	time.Sleep(1 * time.Second)

	// Ensure the container is still starting
	_ = ensureContainerState(t, ctx, client, &ctr, apiv1.ContainerStateStarting)

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err = client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNetwork := ensureNetworkCreated(t, ctx, &net)

	_, inspectedContainer := ensureContainerRunning(t, ctx, &ctr)

	found := slices.ContainsFunc(inspectedContainer.Networks, func(n containers.InspectedContainerNetwork) bool {
		return n.Name == updatedNetwork.Status.NetworkName
	})
	require.True(t, found)
}

// Ensures that network aliases are applied to containers if Container Spec requests them
func TestContainerNetworkWithAliases(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-container-multiple-networks-aliases"

	net1 := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-net1",
			Namespace: metav1.NamespaceNone,
		},
	}

	net2 := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-net2",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork objects '%s' and '%s'", net1.ObjectMeta.Name, net2.ObjectMeta.Name)
	err := client.Create(ctx, &net1)
	require.NoError(t, err, "could not create first ContainerNetwork object")

	err = client.Create(ctx, &net2)
	require.NoError(t, err, "could not create second ContainerNetwork object")

	updatedNetwork1 := ensureNetworkCreated(t, ctx, &net1)
	updatedNetwork2 := ensureNetworkCreated(t, ctx, &net2)

	const imageName = testName + "-image"

	aliases1 := []string{"web", "frontend"}
	aliases2 := []string{"api", "backend"}

	ctr := apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image: imageName,
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name:    testName + "-net1",
					Aliases: aliases1,
				},
				{
					Name:    testName + "-net2",
					Aliases: aliases2,
				},
			},
		},
	}

	t.Logf("Creating Container object '%s' with multiple networks and aliases", ctr.ObjectMeta.Name)
	err = client.Create(ctx, &ctr)
	require.NoError(t, err, "Could not create a Container object")

	_, inspectedContainer := ensureContainerRunning(t, ctx, &ctr)

	var foundNetwork1, foundNetwork2 *containers.InspectedContainerNetwork
	for _, network := range inspectedContainer.Networks {
		switch network.Name {
		case updatedNetwork1.Status.NetworkName:
			foundNetwork1 = &network
		case updatedNetwork2.Status.NetworkName:
			foundNetwork2 = &network
		}
	}

	require.NotNil(t, foundNetwork1, "Expected to find network %s in container networks", updatedNetwork1.Status.NetworkName)
	require.NotNil(t, foundNetwork2, "Expected to find network %s in container networks", updatedNetwork2.Status.NetworkName)

	require.Equal(t, aliases1, foundNetwork1.Aliases, "Network 1 aliases should match expected values")
	require.Equal(t, aliases2, foundNetwork2.Aliases, "Network 2 aliases should match expected values")
}
