/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	ps "github.com/shirou/gopsutil/v4/process"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestNetworkCreateInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-network-create-instance"

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

func TestNetworkRemoveInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-network-remove-instance"

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
	err = retryOnConflict(ctx, net.NamespacedName(), func(ctx context.Context, currentNet *apiv1.ContainerNetwork) error {
		return client.Delete(ctx, currentNet)
	})
	require.NoError(t, err, "could not delete a ContainerNetwork object")

	err = wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(_ context.Context) (bool, error) {
		inspectedNetworks, networkInspectionErr := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{updatedNet.Status.ID}})
		if !errors.Is(networkInspectionErr, containers.ErrNotFound) || len(inspectedNetworks) != 0 {
			return false, nil
		}

		return true, nil
	})
	require.NoError(t, err, "network was not removed")
}

func TestNetworkCreatePersistentInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-network-create-persistent-instance"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkSpec{
			NetworkName: testName,
			Persistent:  true,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	_ = ensureNetworkCreated(t, ctx, &net)
}

func TestNetworkCreateExistingPersistentInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-network-create-existing-persistent-instance"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkSpec{
			NetworkName: testName,
			Persistent:  true,
		},
	}

	id, err := containerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: testName,
	})
	require.NoError(t, err, "could not create a network")

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err = client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNetwork := ensureNetworkCreated(t, ctx, &net)
	require.Equal(t, id, updatedNetwork.Status.ID, "network ID did not match expected value")
}

func TestNetworkRemovePersistentInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-network-remove-persistent-instance"

	net := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkSpec{
			NetworkName: testName,
			Persistent:  true,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err := client.Create(ctx, &net)
	require.NoError(t, err, "could not create a ContainerNetwork object")

	updatedNet := ensureNetworkCreated(t, ctx, &net)

	t.Logf("Deleting ContainerNetwork object '%s'", net.ObjectMeta.Name)
	err = retryOnConflict(ctx, net.NamespacedName(), func(ctx context.Context, currentNet *apiv1.ContainerNetwork) error {
		return client.Delete(ctx, currentNet)
	})
	require.NoError(t, err, "could not delete a ContainerNetwork object")

	ctrl_testutil.WaitObjectDeleted(t, ctx, client, &net)

	inspected, err := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{
		Networks: []string{updatedNet.Status.ID},
	})
	require.NoError(t, err, "could not inspect the network")
	require.Len(t, inspected, 1, "expected to find a single network")
}

func ensureNetworkCreated(t *testing.T, ctx context.Context, network *apiv1.ContainerNetwork) *apiv1.ContainerNetwork {
	updatedNet := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(network), func(currentNet *apiv1.ContainerNetwork) (bool, error) {
		if currentNet.Status.State == apiv1.ContainerNetworkStateFailedToStart {
			return false, fmt.Errorf("network creation failed: %s", currentNet.Status.Message)
		}

		return currentNet.Status.ID != "", nil
	})

	inspectedNetworks, err := containerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{updatedNet.Status.ID}})
	require.NoError(t, err, "could not inspect the network")
	require.Len(t, inspectedNetworks, 1, "expected to find a single network")

	return updatedNet
}

func nonExistentProcess(t *testing.T) process.ProcessTreeItem {
	pps, ppsErr := ps.Processes()
	require.NoError(t, ppsErr, "could not list processes")
	pids := slices.Map[process.Pid_t](pps, func(pp *ps.Process) process.Pid_t {
		return process.Uint32_ToPidT(uint32(pp.Pid))
	})

	for {
		const PID_OFFSET = 1000
		i, randErr := randdata.MakeRandomInt64(math.MaxUint32 - PID_OFFSET)
		require.NoError(t, randErr)
		i += PID_OFFSET

		candidate, candidateErr := process.Int64_ToPidT(i)
		require.NoError(t, candidateErr)

		if !slices.Contains(pids, candidate) {
			return process.ProcessTreeItem{
				Pid:          candidate,
				IdentityTime: time.Now().Add(-time.Minute),
			}
		}
	}
}
