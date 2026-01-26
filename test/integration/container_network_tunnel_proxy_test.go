/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	std_slices "slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	stdproto "google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/apiserver"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/maps"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that ContainerNetworkTunnelProxy can be created and deleted (including finalizer handling).
func TestTunnelProxyCreateDelete(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-create-delete"
	log := testutil.NewLogForTesting(t.Name())

	// Use dedicated test environment because otherwise it is difficult to differentiate
	// between different server proxy processes running in parallel tests.
	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 12393
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel",
					ServerServiceName: "test-server-service",
					ClientServiceName: "test-client-service",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for controller to add finalizer to ContainerNetworkTunnelProxy..")
	tunnelProxyFinalizerName := fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return slices.Contains(tp.ObjectMeta.Finalizers, tunnelProxyFinalizerName), nil
	})

	t.Logf("Deleting ContainerNetworkTunnelProxy object '%s'", tunnelProxy.ObjectMeta.Name)
	err = retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(ctx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		return serverInfo.Client.Delete(ctx, tp)
	})
	require.NoError(t, err, "Could not delete ContainerNetworkTunnelProxy object")

	t.Log("Waiting for controller to remove finalizer and complete deletion...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &tunnelProxy)
}

// Verifies that ContainerNetworkTunnelProxy can be created without an existing ContainerNetwork
// and transitions to Running state once the network is created.
func TestTunnelProxyDelayedNetworkCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-delayed-network-creation"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

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
					Name:              "test-tunnel",
					ServerServiceName: "test-server-service",
					ClientServiceName: "test-client-service",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s' without referenced ContainerNetwork", tunnelProxy.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	// Wait for the controller to do initial pass over the object
	t.Log("Waiting for controller to add finalizer to ContainerNetworkTunnelProxy...")
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStatePending, nil
	})

	const serverControlPort int32 = 13299
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	// Now create the ContainerNetwork that the tunnel proxy references
	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tunnelProxy.Spec.ContainerNetworkName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	createNetworkErr := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, createNetworkErr, "Could not create a ContainerNetwork object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerImage, "Tunnel proxy should publish the image for the client proxy container")
}

// Verifies that running ContainerNetworkTunnelProxy has the status updated with client proxy and server proxy information.
func TestTunnelProxyRunningStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-running-status"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 26444
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel",
					ServerServiceName: "test-server-service",
					ClientServiceName: "test-client-service",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	t.Log("Verifying client proxy status...")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerImage, "Tunnel proxy should publish the image for the client proxy container")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerID, "Tunnel proxy should have a client proxy container ID")
	require.True(t, networking.IsValidPort(int(updatedTunnelProxy.Status.ClientProxyControlPort)), "Tunnel proxy should have a valid client proxy control port")
	require.True(t, networking.IsValidPort(int(updatedTunnelProxy.Status.ClientProxyDataPort)), "Tunnel proxy should have a valid client proxy data port")

	t.Log("Verifying client proxy container exists...")
	inspectedContainers, inspectErr := serverInfo.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedTunnelProxy.Status.ClientProxyContainerID},
	})
	require.NoError(t, inspectErr, "Should be able to inspect client proxy container")
	require.Len(t, inspectedContainers, 1, "Should find exactly one container")
	clientContainer := inspectedContainers[0]
	require.Equal(t, updatedTunnelProxy.Status.ClientProxyContainerImage, clientContainer.Image, "Container should have the expected image")
	require.Equal(t, containers.ContainerStatusRunning, clientContainer.Status, "Container should be running")

	t.Log("Verifying client proxy container has correct labels...")
	require.Contains(t, clientContainer.Labels, controllers.CreatorProcessIdLabel, "Container should have creator process ID label")
	require.Equal(t, fmt.Sprintf("%d", os.Getpid()), clientContainer.Labels[controllers.CreatorProcessIdLabel], "Container should have correct creator process ID label")
	require.Contains(t, clientContainer.Labels, controllers.CreatorProcessStartTimeLabel, "Container should have creator process start time label")

	t.Log("Verifying server proxy status...")
	require.NotNil(t, updatedTunnelProxy.Status.ServerProxyProcessID, "Server proxy should have a process ID")
	require.False(t, updatedTunnelProxy.Status.ServerProxyStartupTimestamp.IsZero(), "Server proxy should have a startup timestamp")
	require.NotEmpty(t, updatedTunnelProxy.Status.ServerProxyStdOutFile, "Server proxy should publish a stdout log file path")
	require.NotEmpty(t, updatedTunnelProxy.Status.ServerProxyStdErrFile, "Server proxy should publish a stderr log file path")
	require.Equal(t, serverControlPort, updatedTunnelProxy.Status.ServerProxyControlPort, "Server proxy should have the expected control port")

	t.Log("Verifying server proxy process has been started...")
	serverPid, pidErr := process.Int64_ToPidT(*updatedTunnelProxy.Status.ServerProxyProcessID)
	require.NoError(t, pidErr, "Should be able to convert process ID")
	pe, found := teInfo.TestProcessExecutor.FindByPid(serverPid)
	require.True(t, found, "Should find server proxy process in test process executor")

	t.Log("Verifying server proxy process has correct launch arguments...")
	require.True(t, len(pe.Cmd.Args) >= 6, "Server proxy should have at least 6 command line arguments")
	require.Equal(t, "tunnel-server", pe.Cmd.Args[1], "First argument should be 'tunnel-server'")
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, pe.Cmd.Args[2], "Second argument should be client control address")
	require.Equal(t, fmt.Sprintf("%d", updatedTunnelProxy.Status.ClientProxyControlPort), pe.Cmd.Args[3], "Third argument should be client control port")
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, pe.Cmd.Args[4], "Fourth argument should be client data address")
	require.Equal(t, fmt.Sprintf("%d", updatedTunnelProxy.Status.ClientProxyDataPort), pe.Cmd.Args[5], "Fifth argument should be client data port")
}

// Verifies that ContainerNetworkTunnelProxy proxy pair cleanup works correctly during object deletion.
func TestTunnelProxyCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-cleanup"
	log := testutil.NewLogForTesting(t.Name())

	controllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, controllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 34567
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel",
					ServerServiceName: "test-server-service",
					ClientServiceName: "test-client-service",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	// Capture resource information before deletion
	clientContainerID := updatedTunnelProxy.Status.ClientProxyContainerID
	serverProcessID := updatedTunnelProxy.Status.ServerProxyProcessID

	t.Log("Verifying resources exist before deletion...")
	require.NotEmpty(t, clientContainerID, "Client proxy container ID should be set")
	require.NotNil(t, serverProcessID, "Server proxy process ID should be set")
	require.Greater(t, *serverProcessID, int64(0), "Server proxy process ID should be valid")

	// Verify the client container exists
	orchestrator := serverInfo.ContainerOrchestrator
	containerInfoList, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{clientContainerID},
	})
	require.NoError(t, inspectErr, "Should be able to inspect client proxy container")
	require.Len(t, containerInfoList, 1, "Client proxy container should exist")
	require.Equal(t, clientContainerID, containerInfoList[0].Id, "Container ID should match")

	t.Logf("Deleting ContainerNetworkTunnelProxy object '%s'", tunnelProxy.ObjectMeta.Name)
	err = retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(ctx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		return serverInfo.Client.Delete(ctx, tp)
	})
	require.NoError(t, err, "Could not delete ContainerNetworkTunnelProxy object")

	t.Log("Waiting for controller to remove finalizer and complete deletion...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &tunnelProxy)

	t.Log("Verifying proxy resources are cleaned up...")

	// Verify the client container has been removed
	_, inspectErrAfter := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{clientContainerID},
	})
	require.ErrorIs(t, inspectErrAfter, containers.ErrNotFound, "Client proxy container should have been removed")

	// For the server process, verify that it was stopped by checking if it has finished in the test process executor
	processExecution, processFound := teInfo.TestProcessExecutor.FindByPid(process.Pid_t(*serverProcessID))
	require.True(t, processFound, "Server proxy process should be found in test process executor")
	require.True(t, processExecution.Finished(), "Server proxy process should have been stopped")
}

// Verifies that a tunnel proxy can be created with a single tunnel, and that this tunnel gets successfully prepared.
func TestTunnelProxyTunnelCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-tunnel-management"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 32965
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	const tunnelCount = 1
	const serverServicePort int32 = 28270
	ts := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName, serverServicePort)
	tunnelData := ts[0]

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", testName)
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              tunnelData.tunnelName,
					ServerServiceName: tunnelData.serverServiceName,
					ClientServiceName: tunnelData.clientServiceName,
				},
			},
		},
	}
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Logf("Waiting for ContainerNetworkTunnelProxy '%s' to transition to Running state and complete tunnel preparation...", tunnelProxy.ObjectMeta.Name)
	updatedProxy := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelCount, apiv1.TunnelStateReady)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, updatedProxy.Status.State, "Tunnel proxy should be in Running state")

	validateTunnel(t, ctx, updatedProxy.Status.TunnelStatuses[0], tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)
}

// Verifies that when tunnel preparation fails after maximum attempts, the tunnel status indicates failure.
func TestTunnelProxyTunnelFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-tunnel-failure"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 33456
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	const tunnelCount = 1
	const serverServicePort int32 = 29381
	ts := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, tunnelCount, testName, serverServicePort)
	tunnelData := ts[0]

	// Disable the tunnel in the test control client to simulate preparation failure
	t.Logf("Disabling tunnel with fingerprint %v to simulate preparation failure", tunnelData.fingerprint)
	disabled := teInfo.TestTunnelControlClient.DisableTunnel(tunnelData.fingerprint)
	require.True(t, disabled, "Expected tunnel to be disabled successfully")

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", testName)
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              tunnelData.tunnelName,
					ServerServiceName: tunnelData.serverServiceName,
					ClientServiceName: tunnelData.clientServiceName,
				},
			},
		},
	}
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Logf("Waiting for ContainerNetworkTunnelProxy '%s' to transition to Running state with failed tunnel...", tunnelProxy.ObjectMeta.Name)
	_ = waitForTunnelFailure(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelData.tunnelName)
}

// Verifies that tunnel status is updated when server Service transitions from having a valid address to not having one.
func TestTunnelProxyServerServiceTransition(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-server-service-transition"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}
	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 34567
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	const tunnelCount = 1
	const serverServicePort int32 = 30123
	tunnelDataList := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, tunnelCount, testName, serverServicePort)
	tunnelData := tunnelDataList[0]

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", testName)
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              tunnelData.tunnelName,
					ServerServiceName: tunnelData.serverServiceName,
					ClientServiceName: tunnelData.clientServiceName,
				},
			},
		},
	}
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Logf("Waiting for ContainerNetworkTunnelProxy '%s' to transition to Running state and complete tunnel preparation...", tunnelProxy.ObjectMeta.Name)
	updatedProxy := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelCount, apiv1.TunnelStateReady)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, updatedProxy.Status.State, "Tunnel proxy should be in Running state")

	t.Logf("Validating tunnel '%s' is initially prepared and live...", tunnelData.tunnelName)
	validateTunnel(t, ctx, updatedProxy.Status.TunnelStatuses[0], tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)

	t.Logf("Deleting server Service Endpoint '%s' to trigger Service transition to NotReady state...", tunnelData.serverServiceEndpointName)
	serverEndpointNamespacedName := types.NamespacedName{Name: tunnelData.serverServiceEndpointName, Namespace: metav1.NamespaceNone}
	err = retryOnConflictEx(ctx, serverInfo.Client, serverEndpointNamespacedName, func(ctx context.Context, endpoint *apiv1.Endpoint) error {
		return serverInfo.Client.Delete(ctx, endpoint)
	})
	require.NoError(t, err, "Could not delete server Service Endpoint")

	t.Logf("Waiting for server Service '%s' to transition to NotReady state...", tunnelData.serverServiceName)
	serverServiceNamespacedName := types.NamespacedName{Name: tunnelData.serverServiceName, Namespace: metav1.NamespaceNone}
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, serverServiceNamespacedName, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateNotReady && svc.Status.EffectiveAddress == "" && svc.Status.EffectivePort == 0, nil
	})

	t.Logf("Waiting for tunnel '%s' to be cleaned up due to server Service losing its effective address and port...", tunnelData.tunnelName)
	validateTunnelDeleted(t, ctx, serverInfo.Client, tunnelData, teInfo.TestTunnelControlClient)

	t.Logf("Verifying ContainerNetworkTunnelProxy '%s' tunnel is no longer ready...", tunnelProxy.ObjectMeta.Name)
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		for _, ts := range tp.Status.TunnelStatuses {
			if ts.Name == tunnelData.tunnelName && ts.State == apiv1.TunnelStateNotReady {
				return true, nil
			}
		}
		return false, nil
	})

	t.Logf("Re-creating server Service Endpoint '%s' to trigger Service transition back to Ready...", tunnelData.serverServiceEndpointName)
	serverEndpoint := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tunnelData.serverServiceEndpointName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceName:      tunnelData.serverServiceName,
			ServiceNamespace: metav1.NamespaceNone,
			Address:          networking.IPv4LocalhostDefaultAddress,
			Port:             serverServicePort,
		},
	}
	err = serverInfo.Client.Create(ctx, &serverEndpoint)
	require.NoError(t, err, "Could not re-create server Service Endpoint '%s'", tunnelData.serverServiceEndpointName)

	t.Logf("Waiting for server Service '%s' to transition back to Ready state...", tunnelData.serverServiceName)
	_ = waitServiceReadyEx(t, ctx, serverInfo.Client, serverServiceNamespacedName)

	t.Logf("Waiting for tunnel '%s' to be prepared again...", tunnelData.tunnelName)
	restoredProxy := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelCount, apiv1.TunnelStateReady)

	t.Logf("Validating tunnel '%s' is restored and live again...", tunnelData.tunnelName)
	validateTunnel(t, ctx, restoredProxy.Status.TunnelStatuses[0], tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)
}

// Verifies that multiple tunnels can be created and deleted for a single ContainerNetworkTunnelProxy object.
func TestTunnelProxyMultipleTunnels(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-multiple-tunnels"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}
	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	networkCreateErr := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, networkCreateErr, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 45890
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	const tunnelCount = 3
	const serverServicePortRangeStart int32 = 40110
	tunnelData := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, tunnelCount, testName, serverServicePortRangeStart)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: func() []apiv1.TunnelConfiguration {
				cfgs := make([]apiv1.TunnelConfiguration, 0, tunnelCount)
				for _, tunnel := range tunnelData {
					cfgs = append(cfgs, apiv1.TunnelConfiguration{
						Name:              tunnel.tunnelName,
						ServerServiceName: tunnel.serverServiceName,
						ClientServiceName: tunnel.clientServiceName,
					})
				}
				return cfgs
			}(),
		},
	}
	t.Logf("Creating ContainerNetworkTunnelProxy '%s' with %d tunnels...", tunnelProxy.ObjectMeta.Name, tunnelCount)
	createProxyErr := serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, createProxyErr, "Could not create a ContainerNetworkTunnelProxy object")

	t.Logf("Waiting for ContainerNetworkTunnelProxy '%s' to be Running with %d ready tunnels...", tunnelProxy.ObjectMeta.Name, tunnelCount)
	updatedProxy := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelCount, apiv1.TunnelStateReady)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, updatedProxy.Status.State, "Tunnel proxy should be in Running state")

	statusByName := maps.SliceToMap(updatedProxy.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Len(t, statusByName, tunnelCount, "Should have status entries for all tunnels")

	for _, td := range tunnelData {
		ts, found := statusByName[td.tunnelName]
		require.True(t, found, "Missing status for tunnel '%s'", td.tunnelName)
		validateTunnel(t, ctx, ts, td, serverInfo.Client, teInfo.TestTunnelControlClient)
	}

	// Delete one of the tunnels and verify tunnel statuses etc. are updated accordingly
	tunnelToDelete := tunnelData[1]
	t.Logf("Deleting tunnel '%s' from ContainerNetworkTunnelProxy '%s'", tunnelToDelete.tunnelName, tunnelProxy.ObjectMeta.Name)
	patchErr := retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(patchCtx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		tunnelProxyPatch := updatedProxy.DeepCopy()
		tunnelProxyPatch.Spec.Tunnels = slices.Select(tunnelProxyPatch.Spec.Tunnels, func(tnc apiv1.TunnelConfiguration) bool {
			return tnc.Name != tunnelToDelete.tunnelName
		})
		return serverInfo.Client.Patch(patchCtx, tunnelProxyPatch, ctrl_client.MergeFromWithOptions(updatedProxy, ctrl_client.MergeFromWithOptimisticLock{}))
	})
	require.NoError(t, patchErr, "Could not patch ContainerNetworkTunnelProxy to remove tunnel")

	t.Logf("Waiting for ContainerNetworkTunnelProxy '%s' to have expected number of TunnelStatuses after tunnel deletion...", tunnelProxy.ObjectMeta.Name)
	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelCount-1, apiv1.TunnelStateReady)

	t.Logf("Verifying tunnel '%s' has been cleaned up...", tunnelToDelete.tunnelName)
	validateTunnelDeleted(t, ctx, serverInfo.Client, tunnelToDelete, teInfo.TestTunnelControlClient)
}

// Verifies that client-side proxy container is created with specified aliases when requested.
func TestTunnelProxyClientProxyAliases(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-client-proxy-aliases"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 14993
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	expectedAliases := []string{"web", "frontend", "proxy"}

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Aliases:              expectedAliases,
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s' with aliases %v", tunnelProxy.ObjectMeta.Name, expectedAliases)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning && tp.Status.ClientProxyContainerID != "", nil
	})

	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerID, "Client proxy container ID should be set")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerImage, "Client proxy container image should be set")

	t.Log("Inspecting client proxy container...")
	inspected, inspectErr := serverInfo.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedTunnelProxy.Status.ClientProxyContainerID},
	})
	require.NoError(t, inspectErr, "Could not inspect client proxy container")
	require.Len(t, inspected, 1, "Expected exactly one inspected container")

	clientProxyContainer := inspected[0]
	require.Equal(t, containers.ContainerStatusRunning, clientProxyContainer.Status, "Client proxy container should be running")
	require.ElementsMatch(t, sorted(expectedAliases), sorted(clientProxyContainer.Networks[0].Aliases))
}

// Verifies that ContainerNetworkTunnelProxy is marked as Failed when server proxy fails to start.
// Also ensures that the client proxy container is removed in that case.
func TestTunnelProxyServerStartupFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-server-startup-failure"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	t.Log("Installing server proxy auto execution with startup error...")
	serverStartAttempted := &atomic.Bool{}
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{os.Args[0], "tunnel-server"},
		},
		StartupError: func(_ *internal_testutil.ProcessExecution) error {
			serverStartAttempted.Store(true)
			return fmt.Errorf("simulated server proxy startup failure")
		},
	})

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel",
					ServerServiceName: "test-server-service",
					ClientServiceName: "test-client-service",
				},
			},
		},
	}

	containerStarted, containerRemoved := watchContainerLifeEvents(t, ctx, serverInfo.ContainerOrchestrator)

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition into Failed state...")
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		if tp.Status.State != apiv1.ContainerNetworkTunnelProxyStateFailed {
			return false, nil
		}
		return serverStartAttempted.Load(), nil
	})

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true /* poll immediately */, func(_ context.Context) (bool, error) {
		return containerStarted.Load() == 1 && containerRemoved.Load() == 1, nil
	})
	require.NoError(t, waitErr, "Expected client proxy container to have been started and then removed")
}

// Verifies that ContainerNetworkTunnelProxy transitions to Failed state when client proxy container cannot be started.
func TestTunnelProxyClientContainerStartupFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-client-container-startup-failure"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, _, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	containerStarted, containerRemoved := watchContainerLifeEvents(t, ctx, serverInfo.ContainerOrchestrator)

	t.Log("Configuring test orchestrator to fail client proxy container startup...")
	tco, isTCO := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTCO, "Container orchestrator should be a TestContainerOrchestrator")
	tco.FailMatchingContainers(ctx, dcptun.ClientProxyContainerImageNamePrefix, 1, "simulated client proxy container startup failure")

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel",
					ServerServiceName: "test-server-service",
					ClientServiceName: "test-client-service",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Failed state...")
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true /* poll immediately */, func(_ context.Context) (bool, error) {
		return containerStarted.Load() == 0 && containerRemoved.Load() == 1, nil
	})
	require.NoError(t, waitErr, "Expected client proxy container to have been removed after failed startup")
}

// Verifies that ContainerNetworkTunnelProxy transitions to Failed state when server proxy process exits unexpectedly.
func TestTunnelProxyServerUnexpectedExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-server-unexpected-exit"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 15678
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel-1",
					ServerServiceName: "test-server-service-1",
					ClientServiceName: "test-client-service-1",
				},
				{
					Name:              "test-tunnel-2",
					ServerServiceName: "test-server-service-2",
					ClientServiceName: "test-client-service-2",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	require.NotNil(t, updatedTunnelProxy.Status.ServerProxyProcessID, "Server proxy process ID should be set")
	serverPID, pidErr := process.Int64_ToPidT(*updatedTunnelProxy.Status.ServerProxyProcessID)
	require.NoError(t, pidErr, "Should be able to convert process ID")

	t.Log("Verify client proxy container exists and is running...")
	ctrs, listErr := serverInfo.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
	require.NoError(t, listErr, "Could not list containers from orchestrator")
	require.Len(t, ctrs, 1, "Expected exactly one container (the client proxy) to exist")
	require.Equal(t, containers.ContainerStatusRunning, ctrs[0].Status, "Client proxy container should be running")

	// NotReady is what we expect the tunnels to be because we have not simulated the corresponding server service for them.
	t.Log("Verifying all tunnels are in NotReady state...")
	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), len(tunnelProxy.Spec.Tunnels), apiv1.TunnelStateNotReady)

	t.Logf("Simulating server proxy process exit with code 3 (PID: %d)...", serverPID)
	teInfo.TestProcessExecutor.SimulateProcessExit(t, serverPID, 3)

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Failed state...")
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})

	t.Log("Verifying client container have been removed...")
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true /* poll immediately */, func(_ context.Context) (bool, error) {
		ctrs, listErr = serverInfo.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
		if listErr != nil {
			return false, listErr
		}
		return len(ctrs) == 0, nil
	})
	require.NoError(t, waitErr, "Expected client proxy container to be removed when ContainerNetworkTunnelProxy entered Failed state")

	t.Log("Verifying all tunnels are in Failed state...")
	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), len(tunnelProxy.Spec.Tunnels), apiv1.TunnelStateFailed)
}

// Verifies that ContainerNetworkTunnelProxy transitions to Failed state when client proxy container unexpectedly stops running.
func TestTunnelProxyClientUnexpectedExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-client-unexpected-exit"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	defer shutdownTestEnvironment(serverInfo, cancel)

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 16789
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "test-tunnel-1",
					ServerServiceName: "test-server-service-1",
					ClientServiceName: "test-client-service-1",
				},
				{
					Name:              "test-tunnel-2",
					ServerServiceName: "test-server-service-2",
					ClientServiceName: "test-client-service-2",
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerID, "Client proxy container ID should be set")
	require.NotNil(t, updatedTunnelProxy.Status.ServerProxyProcessID, "Server proxy process ID should be set")

	t.Log("Verifying client proxy container exists and is running...")
	clientContainerID := updatedTunnelProxy.Status.ClientProxyContainerID
	ctrs, listErr := serverInfo.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
	require.NoError(t, listErr, "Could not list containers from orchestrator")
	require.Len(t, ctrs, 1, "Expected exactly one container (the client proxy) to exist")
	require.Equal(t, clientContainerID, ctrs[0].Id, "Container ID should match")
	require.Equal(t, containers.ContainerStatusRunning, ctrs[0].Status, "Client proxy container should be running")

	t.Log("Verifying server proxy process is running...")
	serverPID, pidErr := process.Int64_ToPidT(*updatedTunnelProxy.Status.ServerProxyProcessID)
	require.NoError(t, pidErr, "Should be able to convert process ID")
	pe, found := teInfo.TestProcessExecutor.FindByPid(serverPID)
	require.True(t, found, "Server proxy process should be found")
	require.False(t, pe.Finished(), "Server proxy process should be running")

	// NotReady is what we expect the tunnels to be because we have not simulated the corresponding server service for them.
	t.Log("Verifying all tunnels are in NotReady state...")
	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), len(tunnelProxy.Spec.Tunnels), apiv1.TunnelStateNotReady)

	t.Logf("Simulating client proxy container exit with code 5 (container ID: %s)...", clientContainerID)
	tco, isTCO := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTCO, "Container orchestrator should be a TestContainerOrchestrator")
	simulateErr := tco.SimulateContainerExit(ctx, clientContainerID, 5)
	require.NoError(t, simulateErr, "Should be able to simulate container exit")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Failed state...")
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})

	t.Log("Verifying server proxy process has been stopped...")
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true /* poll immediately */, func(_ context.Context) (bool, error) {
		pe, found = teInfo.TestProcessExecutor.FindByPid(serverPID)
		if !found {
			return false, fmt.Errorf("server proxy process not found")
		}
		return pe.Finished(), nil
	})
	require.NoError(t, waitErr, "Expected server proxy process to have been stopped when ContainerNetworkTunnelProxy entered Failed state")

	t.Log("Verifying all tunnels are in Failed state...")
	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), len(tunnelProxy.Spec.Tunnels), apiv1.TunnelStateFailed)
}

// Verifies that a ContainerNetworkTunnelProxy really works with real container orchestrator (Docker or Podman).
// This is an advanced test that is not included in routine test runs.
// Requires DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR environment variable to be set to "true".
func TestTunnelProxyWithRealOrchestrator(t *testing.T) {
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)

	t.Parallel()

	const testTimeout = 6 * time.Minute
	const parrotTimeout = 3 * time.Minute

	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()
	const testName = "test-tunnel-proxy-with-real-orchestrator"
	log := testutil.NewLogForTesting(t.Name())
	dcppaths.EnableTestPathProbing()

	serverInfo, pe, startupErr := StartAdvancedTestEnvironment(ctx, AllControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")
	t.Logf("API server started with PID %d", serverInfo.ApiServerPID)
	defer pe.Dispose()
	defer shutdownAdvancedTestEnvironment(t, ctx, cancel, serverInfo)

	serverSvc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parrot-server-service",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}

	t.Logf("Creating Service '%s'", serverSvc.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, serverSvc)
	require.NoError(t, err, "Could not create the parrot server Service")

	const parrotConversations = 5
	parrotToolDir, toolDirErr := internal_testutil.GetTestToolDir("parrot")
	require.NoError(t, toolDirErr, "Could not get parrot tool directory")
	parrotToolPath := filepath.Join(parrotToolDir, "parrot")
	if osutil.IsWindows() {
		parrotToolPath += ".exe"
	}

	serverExe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parrot-server",
			Namespace: metav1.NamespaceNone,
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: fmt.Sprintf(`[{"serviceName":"%s"}]`, serverSvc.ObjectMeta.Name),
			},
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: parrotToolPath,
			Args: []string{
				"server",
				"--port", fmt.Sprintf(`{{- portForServing "%s" -}}`, serverSvc.ObjectMeta.Name),
				"--conversations", fmt.Sprintf("%d", parrotConversations),
				"--timeout", parrotTimeout.String(),
			},
		},
	}

	t.Logf("Creating Executable '%s'", serverExe.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, serverExe)
	require.NoError(t, err, "Could not create the parrot server Executable")

	clientSvc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parrot-client-service",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}

	t.Logf("Creating Service '%s'", clientSvc.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, clientSvc)
	require.NoError(t, err, "Could not create the parrot client Service")

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const proxyAlias = "parrot"
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "parrot-tunnel",
					ServerServiceName: serverSvc.Name,
					ClientServiceName: clientSvc.Name,
				},
			},
			Aliases: []string{proxyAlias},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	// Compare with PARROT_TOOL_CONTAINER_BINARY in Makefile
	parrotContainerBinaryPath := filepath.Join(parrotToolDir, "parrot_c")

	parrotImage, parrotContainerPath, parrotImageErr := ensureParrotContainerImage(ctx, parrotContainerBinaryPath, serverInfo.ContainerOrchestrator)
	require.NoError(t, parrotImageErr, "Could not ensure parrot container image")

	clientCtr := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "parrot-client",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			Image:   parrotImage,
			Command: parrotContainerPath,
			Args: []string{
				"client",
				"--address", proxyAlias,
				"--port", fmt.Sprintf(`{{- portFor "%s" -}}`, clientSvc.ObjectMeta.Name),
				"--conversations", fmt.Sprintf("%d", parrotConversations),
				"--timeout", parrotTimeout.String(),
			},
			Networks: &[]apiv1.ContainerNetworkConnectionConfig{
				{
					Name: network.Name,
				},
			},
			// Enable ability to do pings etc. from within container, for debugging purposes.
			RunArgs: []string{"--cap-add=NET_RAW"},
		},
	}

	t.Logf("Creating parrot client Container '%s'...", clientCtr.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, clientCtr)
	require.NoError(t, err, "Could not create the parrot client Container")

	t.Log("Waiting for parrot client container to complete...")
	updatedClientCtr := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, clientCtr.NamespacedName(), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited && c.Status.ExitCode != nil, nil
	})
	require.Equal(t, int32(0), *updatedClientCtr.Status.ExitCode, "Parrot client container should exit with code 0 indicating successful completion of all conversations")

	t.Log("Waiting for parrot server executable to complete...")
	updatedServerExe := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, serverExe.NamespacedName(), func(e *apiv1.Executable) (bool, error) {
		return e.Status.State == apiv1.ExecutableStateFinished && e.Status.ExitCode != nil, nil
	})
	require.Equal(t, int32(0), *updatedServerExe.Status.ExitCode, "Parrot server executable should exit with code 0 indicating successful completion of all conversations")
}

func shutdownTestEnvironment(serverInfo *ctrl_testutil.ApiServerInfo, cancel context.CancelFunc) {
	serverInfo.Dispose()

	// Wait for the API server cleanup to complete (with timeout)
	select {
	case <-serverInfo.ApiServerDisposalComplete.Wait():
	case <-time.After(20 * time.Second):
	}

	cancel()
}

func shutdownAdvancedTestEnvironment(
	t *testing.T,
	ctx context.Context,
	cancel context.CancelFunc,
	serverInfo *ctrl_testutil.ApiServerInfo,
) {
	req, reqCreationErr := ctrl_testutil.MakeResourceCleanupRequest(ctx, serverInfo)
	require.NoError(t, reqCreationErr)

	client := ctrl_testutil.GetApiServerClient(t, serverInfo)
	resp, respErr := client.Do(req)
	require.NoError(t, respErr, "Failed to submit request to start resource cleanup")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	t.Logf("Waiting for API server to complete cleanup...")
	waitErr := ctrl_testutil.WaitApiServerStatus(ctx, client, serverInfo, apiserver.ApiServerCleanupComplete)
	require.NoError(t, waitErr, "Failed to wait for API server to complete cleanup")

	shutdownTestEnvironment(serverInfo, cancel)
}

// The tunnel controller will try to create a server-side proxy
// and read the network configuration (control port in particular) off of it,
// so we need to simulate that.
func simulateServerProxy(
	t *testing.T,
	serverControlPort int32,
	tpe *internal_testutil.TestProcessExecutor,
) {
	tpe.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"integration.test", "tunnel-server"},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			tc := dcptun.TunnelProxyConfig{
				ServerControlPort: serverControlPort,
			}
			tcBytes, tcErr := json.Marshal(tc)
			require.NoError(t, tcErr, "Could not marshal TunnelProxyConfig to JSON??")
			_, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(tcBytes))
			require.NoError(t, writeErr, "Could not write TunnelProxyConfig to stdout")

			<-pe.Signal
			return 0
		},
	})
}

type testTunnelData struct {
	tunnelName                  string
	serverServiceName           string
	serverServiceEndpointName   string
	clientServiceName           string
	clientServiceNamespacedName types.NamespacedName
	fingerprint                 dcptunproto.TunnelRequestFingerprint
}

// Creates Services, Endpoints, and prepares tunnel control client
// to simulate multiple tunnels for testing.
func prepareTunnelServices(
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	tunnelControlClient *ctrl_testutil.TestTunnelControlClient,
	count int,
	testName string,
	serverServicePortRangeStart int32,
) []testTunnelData {
	var tunnels []testTunnelData

	for i := 0; i < count; i++ {
		tunnelName := fmt.Sprintf("%s-tunnel-%d", testName, i)
		serverSvcName := fmt.Sprintf("%s-server-service-%d", testName, i)
		clientSvcName := fmt.Sprintf("%s-client-service-%d", testName, i)
		serverEndpointName := fmt.Sprintf("%s-server-endpoint-%d", testName, i)
		serverServicePort := serverServicePortRangeStart + int32(i)

		// Server Service (proxyless address allocation)
		serverSvc := apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serverSvcName,
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol:              apiv1.TCP,
				AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
			},
		}
		t.Logf("Creating server Service '%s' for tunnel %d", serverSvcName, i)
		createServerSvcErr := apiClient.Create(ctx, &serverSvc)
		require.NoError(t, createServerSvcErr, "Could not create server Service '%s'", serverSvcName)

		// Server Endpoint (represents server-side listening port)
		serverEndpoint := apiv1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serverEndpointName,
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.EndpointSpec{
				ServiceName:      serverSvc.ObjectMeta.Name,
				ServiceNamespace: serverSvc.ObjectMeta.Namespace,
				Address:          networking.IPv4LocalhostDefaultAddress,
				Port:             serverServicePort,
			},
		}
		t.Logf("Creating server Endpoint '%s' for tunnel %d", serverEndpointName, i)
		createServerEndpointErr := apiClient.Create(ctx, &serverEndpoint)
		require.NoError(t, createServerEndpointErr, "Could not create server Endpoint '%s'", serverEndpointName)

		// Not waiting for server Service to become ready because the tunnel proxy controller SHOULD handle that internally.

		// Client Service
		clientSvc := apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clientSvcName,
				Namespace: metav1.NamespaceNone,
			},
			Spec: apiv1.ServiceSpec{
				Protocol:              apiv1.TCP,
				AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
			},
		}
		t.Logf("Creating client Service '%s' for tunnel %d", clientSvcName, i)
		createClientSvcErr := apiClient.Create(ctx, &clientSvc)
		require.NoError(t, createClientSvcErr, "Could not create client Service '%s'", clientSvcName)

		// Enable tunnel in test control client so controller can prepare it
		tunnelSpec := &dcptunproto.TunnelSpec{
			ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:    stdproto.Int32(serverServicePort),
		}
		fingerprint := ctrl_testutil.TestFingerprint(tunnelSpec)
		tunnelControlClient.EnableTunnel(tunnelSpec)

		// Add tunnel data to slice
		tunnels = append(tunnels, testTunnelData{
			tunnelName:                  tunnelName,
			serverServiceName:           serverSvcName,
			serverServiceEndpointName:   serverEndpointName,
			clientServiceName:           clientSvcName,
			clientServiceNamespacedName: clientSvc.NamespacedName(),
			fingerprint:                 fingerprint,
		})
	}

	return tunnels
}

func validateTunnel(
	t *testing.T,
	ctx context.Context,
	ts apiv1.TunnelStatus,
	td testTunnelData,
	apiClient ctrl_client.Client,
	tunnelControlClient *ctrl_testutil.TestTunnelControlClient,
) {
	require.False(t, ts.Timestamp.IsZero(), "Tunnel '%s' should have timestamp", td.tunnelName)
	require.ElementsMatch(t, ts.ClientProxyAddresses, []string{networking.IPv4LocalhostDefaultAddress}, "Tunnel '%s' should have expected client proxy address", td.tunnelName)

	// Verify tunnel spec recorded by control client has a valid ID and matches status
	updatedTunnelSpec := tunnelControlClient.GetTunnelSpec(td.fingerprint)
	require.NotNil(t, updatedTunnelSpec, "Tunnel spec should exist for '%s'", td.tunnelName)
	id := updatedTunnelSpec.GetTunnelRef().GetTunnelId()
	require.True(t, id > ctrl_testutil.InvalidTunnelID, "Tunnel '%s' should have valid tunnel ID", td.tunnelName)
	require.Equal(t, id, ts.TunnelID, "Tunnel '%s' status should have matching tunnel ID", td.tunnelName)

	// Verify client service endpoint exists and port matches status.ClientProxyPort
	t.Logf("Verifying client Service endpoint for tunnel '%s' (service '%s')...", td.tunnelName, td.clientServiceName)
	clientEndpoint := waitEndpointExistsEx(t, ctx, apiClient, "could not find endpoint for service "+td.clientServiceName, func(e *apiv1.Endpoint) (bool, error) {
		return e.Spec.ServiceName == td.clientServiceName, nil
	})
	require.Equal(t, clientEndpoint.Spec.Port, ts.ClientProxyPort, "Client service endpoint should have expected port for tunnel '%s'", td.tunnelName)

	t.Logf("Waiting for Service '%s' to become ready...", td.clientServiceName)
	_ = waitServiceReadyEx(t, ctx, apiClient, td.clientServiceNamespacedName)
}

func validateTunnelDeleted(
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	td testTunnelData,
	tunnelControlClient *ctrl_testutil.TestTunnelControlClient,
) {
	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		ts := tunnelControlClient.GetTunnelSpec(td.fingerprint)
		return ts.GetTunnelRef().GetTunnelId() == ctrl_testutil.InvalidTunnelID, nil
	})
	require.NoError(t, err, "Timed out waiting for tunnel controller to delete tunnel '%s'", td.tunnelName)

	t.Logf("Verifying client Service for tunnel '%s' has no endpoints...", td.tunnelName)
	waitEndpointCountEx(t, ctx, apiClient, td.clientServiceName, 0)

	t.Logf("Verifying client Service for tunnel '%s' is in 'NotReady' state...", td.tunnelName)
	_ = waitObjectAssumesStateEx(t, ctx, apiClient, td.clientServiceNamespacedName, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateNotReady, nil
	})
}

func waitAllTunnelsInState(
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	tunnelProxyName types.NamespacedName,
	expectedCount int,
	desiredState apiv1.TunnelState,
) *apiv1.ContainerNetworkTunnelProxy {
	return waitObjectAssumesStateEx(t, ctx, apiClient, tunnelProxyName, func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		// Make sure we have ONLY expectedCount tunnels before checking whether they are all in expected state
		if len(tp.Status.TunnelStatuses) != expectedCount {
			return false, nil
		}

		allInDesiredState := slices.All(tp.Status.TunnelStatuses, func(ts apiv1.TunnelStatus) bool {
			return ts.State == desiredState
		})

		return allInDesiredState, nil
	})
}

func waitForTunnelFailure(
	t *testing.T,
	ctx context.Context,
	apiClient ctrl_client.Client,
	tunnelProxyName types.NamespacedName,
	tunnelName string,
) *apiv1.ContainerNetworkTunnelProxy {
	return waitObjectAssumesStateEx(t, ctx, apiClient, tunnelProxyName, func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		if tp.Status.State != apiv1.ContainerNetworkTunnelProxyStateRunning {
			return false, nil
		}

		// Look for the specific tunnel and check if it has failed
		for _, ts := range tp.Status.TunnelStatuses {
			if ts.Name == tunnelName && ts.State == apiv1.TunnelStateFailed {
				require.False(t, ts.Timestamp.IsZero(), "Tunnel status should have timestamp")
				return true, nil
			}
		}

		return false, nil
	})
}

func sorted[S ~[]E, E cmp.Ordered](s S) S {
	result := std_slices.Clone(s)
	std_slices.Sort(result)
	return result
}

func watchContainerLifeEvents(
	t *testing.T,
	ctx context.Context,
	co containers.ContainerOrchestrator,
) (created, deleted *atomic.Uint32) {
	ctrEvtCh := concurrency.NewUnboundedChan[containers.EventMessage](ctx)
	sub, subErr := co.WatchContainers(ctrEvtCh.In)
	require.NoError(t, subErr, "Could not subscribe to container events")

	containerStarted := &atomic.Uint32{}
	containerRemoved := &atomic.Uint32{}

	go func() {
		defer sub.Cancel()

		for {
			select {

			case <-ctx.Done():
				return

			case evt, ok := <-ctrEvtCh.Out:
				if !ok {
					return
				}
				if evt.Source == containers.EventSourceContainer {
					if evt.Action == containers.EventActionStart {
						containerStarted.Add(1)
					}
					if evt.Action == containers.EventActionDestroy {
						containerRemoved.Add(1)
					}
				}
			}
		}
	}()

	return containerStarted, containerRemoved
}

// Ensures that a container image for the parrot utility is built.
// Returns the full image name with tag and the path to parrot binary inside the container.
func ensureParrotContainerImage(
	ctx context.Context,
	parrotBinaryPath string,
	ior containers.ImageOrchestrator,
) (string, string, error) {
	const parrotContainerPath = "/usr/local/bin/parrot"
	dateTag := time.Now().Format("20060102")
	imageName := fmt.Sprintf("dcp_parrot_test_utility:%s", dateTag)

	// Check if image already exists - if so, assume it's fresh
	images, inspectErr := ior.InspectImages(ctx, containers.InspectImagesOptions{
		Images: []string{imageName},
	})
	if inspectErr == nil && len(images) > 0 {
		return imageName, parrotContainerPath, nil
	}

	// Create temporary directory for build context
	tempDir, tempDirErr := os.MkdirTemp("", "parrot-build-*")
	if tempDirErr != nil {
		return "", "", fmt.Errorf("failed to create temporary directory: %w", tempDirErr)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Copy the parrot binary to build context
	destBinaryPath := filepath.Join(tempDir, "parrot")
	if copyErr := copyFileForImageBuild(parrotBinaryPath, destBinaryPath); copyErr != nil {
		return "", "", fmt.Errorf("failed to copy parrot binary to build context: %w", copyErr)
	}

	// Create Dockerfile content

	dockerfileContent := fmt.Sprintf(`
FROM %s

# Copy the parrot binary
COPY --chmod=0755 parrot %[2]s

# Set the entrypoint to the parrot binary
ENTRYPOINT ["%[2]s"]
`, "busybox:latest", parrotContainerPath)
	// Using busybox as the base image helps with debugging.

	// Write Dockerfile
	dockerfilePath := filepath.Join(tempDir, "Dockerfile")
	if writeErr := os.WriteFile(dockerfilePath, []byte(dockerfileContent), osutil.PermissionOnlyOwnerReadWrite); writeErr != nil {
		return "", "", fmt.Errorf("failed to write Dockerfile: %w", writeErr)
	}

	// Build the image
	buildOptions := containers.BuildImageOptions{
		ContainerBuildContext: &apiv1.ContainerBuildContext{
			Context:    tempDir,
			Dockerfile: dockerfilePath,
			Tags:       []string{imageName},
		},
	}

	buildErr := ior.BuildImage(ctx, buildOptions)
	if buildErr != nil {
		return "", "", fmt.Errorf("failed to build parrot container image: %w", buildErr)
	}

	return imageName, parrotContainerPath, nil
}

// copyFileForImageBuild copies a file from src to dst for image building purposes
func copyFileForImageBuild(src, dst string) error {
	sourceFile, sourceErr := os.Open(src)
	if sourceErr != nil {
		return sourceErr
	}
	defer func() { _ = sourceFile.Close() }()

	destFile, destErr := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, osutil.PermissionOnlyOwnerReadWriteExecute)
	if destErr != nil {
		return destErr
	}

	if _, copyErr := io.Copy(destFile, sourceFile); copyErr != nil {
		_ = destFile.Close()
		return copyErr
	}

	return destFile.Close()
}
