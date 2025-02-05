package integration_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/tklauser/ps"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stretchr/testify/require"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
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

func TestCreatePersistentNetworkInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-create-persistent-network-instance"

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

func TestCreateExistingPersistentNetworkInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-create-existing-persistent-network-instance"

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

func TestRemovePersistentNetworkInstance(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const testName = "test-remove-persistent-network-instance"

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

func TestUnusedNetworkHarvesting(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	log := ctrl.Log.WithName("TestUnusedNetworkHarvesting")
	co, coErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, coErr, "could not create a test container orchestrator")
	defer require.NoError(t, co.Close())

	procNonExistent := nonExistentProcess(t)

	const prefix = "unused-network-harvesting-"

	// Network with no DCP labels (should be preserved)
	const netNoLabels = prefix + "no-labels"
	_, netCreateErr := co.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: netNoLabels})
	require.NoError(t, netCreateErr)

	// Persistent DCP network (should be preserved)
	const netPersistent = prefix + "persistent"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netPersistent,
		Labels: map[string]string{
			controllers.PersistentNetworkLabel:       "true",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	procThis, procThisErr := process.This()
	require.NoError(t, procThisErr)

	// Network that is used by existing process (should be preserved)
	const netUsedByExisting = prefix + "used-by-existing"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netUsedByExisting,
		Labels: map[string]string{
			controllers.PersistentNetworkLabel:       "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procThis.Pid),
			controllers.CreatorProcessStartTimeLabel: procThis.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	// Network that has containers attached to it (should be preserved)
	const netWithContainers = prefix + "with-containers"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netWithContainers,
		Labels: map[string]string{
			controllers.PersistentNetworkLabel:       "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)
	_, containerCreateErr := co.RunContainer(ctx, containers.RunContainerOptions{
		Name:    prefix + "container-1",
		Network: netWithContainers,
	})
	require.NoError(t, containerCreateErr)
	_, containerCreateErr = co.RunContainer(ctx, containers.RunContainerOptions{
		Name:    prefix + "container-2",
		Network: netWithContainers,
	})
	require.NoError(t, containerCreateErr)

	// Network that should be removed
	const netToRemove = prefix + "to-remove"
	_, netCreateErr = co.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: netToRemove,
		Labels: map[string]string{
			controllers.PersistentNetworkLabel:       "false",
			controllers.CreatorProcessIdLabel:        fmt.Sprintf("%d", procNonExistent.Pid),
			controllers.CreatorProcessStartTimeLabel: procNonExistent.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		},
	})
	require.NoError(t, netCreateErr)

	harvestErr := controllers.DoHarvestUnusedNetworks(ctx, co, log)
	require.NoError(t, harvestErr, "could not harvest unused networks")

	remaining, listNetworksErr := co.ListNetworks(ctx)
	require.NoError(t, listNetworksErr, "could not list networks")
	remainingNames := slices.Map[containers.ListedNetwork, string](remaining, func(n containers.ListedNetwork) string {
		return n.Name
	})
	require.ElementsMatch(t, []string{netNoLabels, netPersistent, netUsedByExisting, netWithContainers, co.DefaultNetworkName(), "host", "none"}, remainingNames, "unexpected networks remaining")
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
	pids := slices.Map[ps.Process, process.Pid_t](pps, func(pp ps.Process) process.Pid_t {
		pid, pidErr := process.IntToPidT(pp.PID())
		require.NoError(t, pidErr)
		return pid
	})

	for {
		const PID_OFFSET = 1000
		i, randErr := randdata.MakeRandomInt64(math.MaxUint32 - PID_OFFSET)
		require.NoError(t, randErr)
		i += PID_OFFSET

		candidate, candidateErr := process.Int64ToPidT(i)
		require.NoError(t, candidateErr)

		if !slices.Contains(pids, candidate) {
			return process.ProcessTreeItem{
				Pid:          candidate,
				CreationTime: time.Now().Add(-time.Minute),
			}
		}
	}
}
