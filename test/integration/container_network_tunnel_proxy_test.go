/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	std_maps "maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	std_slices "slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/apiserver"
	"github.com/microsoft/dcp/internal/containers"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/containertest"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	usvc_io "github.com/microsoft/dcp/pkg/io"
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

	// Use dedicated test environment because otherwise it is difficult to differentiate
	// between different server proxy processes running in parallel tests.
	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

// Verifies that ContainerNetworkTunnelProxy publishes proxy status and loopback ports,
// while keeping runtime credentials in private files and stdin rather than arguments or API payloads.
func TestTunnelProxyRunningStatus(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-running-status"

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")
	testContainerOrchestrator, ok := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, ok)
	buildContextDir := teInfo.SessionFolder
	imagePlan, imagePlanErr := dcptun.PrepareClientProxyImageBuild(ctx, buildContextDir)
	require.NoError(t, imagePlanErr)
	require.NoError(t, os.Remove(imagePlan.BuildContextArchive.Source))
	releaseImageBuild := testContainerOrchestrator.BlockBuildImage(imagePlan.Image)
	defer releaseImageBuild()

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")
	updatedNetwork := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, network.NamespacedName(), func(currentNet *apiv1.ContainerNetwork) (bool, error) {
		if currentNet.Status.State == apiv1.ContainerNetworkStateFailedToStart {
			return false, fmt.Errorf("network creation failed: %s", currentNet.Status.Message)
		}

		return currentNet.Status.State == apiv1.ContainerNetworkStateRunning && currentNet.Status.NetworkName != "", nil
	})

	const serverControlPort int32 = 26444
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	aliases := []string{"client-proxy", "tunnel-client"}
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Aliases:              aliases,
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

	secondTunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-second",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
		},
	}
	err = serverInfo.Client.Create(ctx, &secondTunnelProxy)
	require.NoError(t, err, "Could not create a second ContainerNetworkTunnelProxy object")

	t.Log("Waiting for physical containers to be created while the shared image build is blocked...")
	waitContainersErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		physicalContainers := apiv2.PhysicalContainerList{}
		listErr := serverInfo.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
		return len(physicalContainers.Items) == 2, listErr
	})
	require.NoError(t, waitContainersErr)
	waitingTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateBuildingImage && tp.Status.Message != "", nil
	})
	require.Contains(t, waitingTunnelProxy.Status.Message, "Waiting for")
	waitBuildErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return testContainerOrchestrator.BuildImageCallCount(imagePlan.Image) >= 1, nil
	})
	require.NoError(t, waitBuildErr)
	require.Equal(t, 1, testContainerOrchestrator.BuildImageCallCount(imagePlan.Image))
	releaseImageBuild()

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, secondTunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	t.Log("Verifying client proxy status...")
	require.Empty(t, updatedTunnelProxy.Status.Message, "Running tunnel proxy should not retain a startup wait message")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerImage, "Tunnel proxy should publish the image for the client proxy container")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerID, "Tunnel proxy should have a client proxy container ID")
	require.True(t, networking.IsValidPort(int(updatedTunnelProxy.Status.ClientProxyControlPort)), "Tunnel proxy should have a valid client proxy control port")
	require.True(t, networking.IsValidPort(int(updatedTunnelProxy.Status.ClientProxyDataPort)), "Tunnel proxy should have a valid client proxy data port")

	t.Log("Verifying V2 physical resources represent the client proxy...")
	physicalImages := apiv2.PhysicalContainerImageList{}
	listImagesErr := serverInfo.Client.List(ctx, &physicalImages, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listImagesErr)
	require.Len(t, physicalImages.Items, 1)
	require.Equal(t, controllers.V1TunnelProxyPhysicalContainerImageName, physicalImages.Items[0].Name)
	require.Equal(t, updatedTunnelProxy.Status.ClientProxyContainerImage, physicalImages.Items[0].Spec.Image.Image)
	require.Equal(t, apiv2.PullPolicyBestEffort, physicalImages.Items[0].Spec.Image.PullPolicy)
	require.Equal(t, apiv2.BuildPolicyIfNeeded, physicalImages.Items[0].Spec.Image.BuildPolicy)
	require.NotNil(t, physicalImages.Items[0].Spec.Image.Build)
	require.Equal(t, []string{dcptun.DefaultBaseImage}, physicalImages.Items[0].Spec.Image.Build.BaseImages)
	require.NotEmpty(t, physicalImages.Items[0].Spec.Image.Build.Digest)
	require.NotNil(t, physicalImages.Items[0].Spec.Image.Build.ContextArchive)
	require.NotEmpty(t, physicalImages.Items[0].Spec.Image.Build.ContextArchive.Source)
	require.NotEmpty(t, physicalImages.Items[0].Spec.Image.Build.ContextArchive.SHA256)
	require.Empty(t, physicalImages.Items[0].Spec.Image.Build.ContextArchive.RawContents)
	require.FileExists(t, physicalImages.Items[0].Spec.Image.Build.ContextArchive.Source)
	buildContextFiles, globErr := filepath.Glob(filepath.Join(buildContextDir, "dcptun-build-context-*.tar"))
	require.NoError(t, globErr)
	require.Equal(t, []string{physicalImages.Items[0].Spec.Image.Build.ContextArchive.Source}, buildContextFiles)
	teInfo.addAfterShutdown(func() {
		require.NoFileExists(t, physicalImages.Items[0].Spec.Image.Build.ContextArchive.Source)
	})
	require.Equal(t, apiv2.PhysicalContainerImagePhaseReady, physicalImages.Items[0].Status.Phase)
	require.Equal(t, 1, testContainerOrchestrator.BuildImageCallCount(updatedTunnelProxy.Status.ClientProxyContainerImage))

	physicalContainers := apiv2.PhysicalContainerList{}
	listContainersErr := serverInfo.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listContainersErr)
	require.Len(t, physicalContainers.Items, 2)
	foundClientPhysicalContainer := false
	clientPhysicalContainerName := ""
	var clientPhysicalContainer *apiv2.PhysicalContainer
	for _, physicalContainer := range physicalContainers.Items {
		require.Equal(t, controllers.V1TunnelProxyPhysicalContainerImageName, physicalContainer.Spec.Container.ImageRef)
		require.Equal(t, apiv2.PhysicalContainerPhaseRunning, physicalContainer.Status.Phase)
		if physicalContainer.Status.ContainerID == updatedTunnelProxy.Status.ClientProxyContainerID {
			foundClientPhysicalContainer = true
			clientPhysicalContainerName = physicalContainer.Name
			clientPhysicalContainer = physicalContainer.DeepCopy()
		}
	}
	require.True(t, foundClientPhysicalContainer)
	require.NotNil(t, clientPhysicalContainer)
	require.Contains(t, clientPhysicalContainer.Spec.Container.Command, "--control-security-file")
	require.NotContains(t, clientPhysicalContainer.Spec.Container.Command, "--ca-cert")
	require.NotContains(t, clientPhysicalContainer.Spec.Container.Command, "--server-cert")
	require.NotContains(t, clientPhysicalContainer.Spec.Container.Command, "--server-key")
	require.Len(t, clientPhysicalContainer.Spec.Container.CreateFiles, 1)
	require.Len(t, clientPhysicalContainer.Spec.Container.CreateFiles[0].Entries, 1)
	securityEntry := clientPhysicalContainer.Spec.Container.CreateFiles[0].Entries[0]
	require.NotEmpty(t, securityEntry.Source)
	require.Empty(t, securityEntry.Contents)
	require.Empty(t, securityEntry.RawContents)
	require.Equal(t, os.FileMode(0o600), securityEntry.Mode)
	require.NotEqual(t, buildContextDir, filepath.Dir(securityEntry.Source), "Credentials must not follow the image-build directory override")
	require.Equal(t, filepath.Clean(usvc_io.DcpTempDir()), filepath.Dir(filepath.Dir(securityEntry.Source)))
	requireTunnelProxyCredentialPermissions(t, securityEntry.Source)
	clientSecurityBytes, clientSecurityReadErr := os.ReadFile(securityEntry.Source)
	require.NoError(t, clientSecurityReadErr)
	var clientSecurity dcptun.TunnelControlSecurityConfig
	require.NoError(t, json.Unmarshal(clientSecurityBytes, &clientSecurity))
	require.NoError(t, clientSecurity.ValidateServer())
	require.Error(t, clientSecurity.ValidateClient())
	require.Empty(t, clientSecurity.PeerCACertBase64)
	require.Empty(t, clientSecurity.OutgoingToken)
	teInfo.addAfterShutdown(func() {
		require.NoFileExists(t, securityEntry.Source)
		require.NoDirExists(t, filepath.Dir(securityEntry.Source))
	})

	t.Log("Verifying client proxy ports are published on the loopback interface only...")
	clientProxyPorts := clientPhysicalContainer.Spec.Container.Ports
	require.Len(t, clientProxyPorts, 2)
	for _, containerPort := range []int32{dcptun.DefaultContainerProxyControlPort, dcptun.DefaultContainerProxyDataPort} {
		portIndex := std_slices.IndexFunc(clientProxyPorts, func(port apiv2.ContainerPort) bool {
			return port.ContainerPort == containerPort
		})
		require.NotEqual(t, -1, portIndex, "Client proxy container should publish port %d", containerPort)
		require.Equal(t, networking.IPv4LocalhostDefaultAddress, clientProxyPorts[portIndex].HostIP,
			"Client proxy port %d should be published on the loopback interface only", containerPort)
	}

	physicalNetworks := apiv2.PhysicalContainerNetworkList{}
	listNetworksErr := serverInfo.Client.List(ctx, &physicalNetworks, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listNetworksErr)
	require.Len(t, physicalNetworks.Items, 1)
	for _, physicalNetwork := range physicalNetworks.Items {
		require.Equal(t, updatedNetwork.Status.ID, physicalNetwork.Spec.NetworkID)
		require.Equal(t, apiv2.PhysicalContainerNetworkPhaseReady, physicalNetwork.Status.Phase)
		require.Equal(t, updatedNetwork.Status.ID, physicalNetwork.Status.NetworkID)
		require.Equal(t, updatedNetwork.Name, physicalNetwork.Annotations[controllers.V1ContainerNetworkNameAnnotation])
		require.Len(t, physicalNetwork.Status.ContainerIDs, 2)
		require.True(t, std_slices.Contains(physicalNetwork.Status.ContainerIDs, updatedTunnelProxy.Status.ClientProxyContainerID))
	}

	physicalConnections := apiv2.PhysicalContainerNetworkConnectionList{}
	listConnectionsErr := serverInfo.Client.List(ctx, &physicalConnections, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listConnectionsErr)
	require.Len(t, physicalConnections.Items, 2)
	foundClientPhysicalConnection := false
	for _, physicalConnection := range physicalConnections.Items {
		if physicalConnection.Spec.ContainerRef == clientPhysicalContainerName {
			foundClientPhysicalConnection = true
			require.Equal(t, aliases, physicalConnection.Spec.Aliases)
		}
	}
	require.True(t, foundClientPhysicalConnection)

	t.Log("Verifying client proxy container exists...")
	inspectedContainers, inspectErr := serverInfo.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedTunnelProxy.Status.ClientProxyContainerID},
	})
	require.NoError(t, inspectErr, "Should be able to inspect client proxy container")
	require.Len(t, inspectedContainers, 1, "Should find exactly one container")
	clientContainer := inspectedContainers[0]
	require.Equal(t, physicalImages.Items[0].Status.ImageID, clientContainer.Image, "Container should use the physical image ID")
	require.Equal(t, containers.ContainerStatusRunning, clientContainer.Status, "Container should be running")
	require.Len(t, clientContainer.Networks, 1, "Client proxy container should only be attached to the target network")
	require.Equal(t, updatedNetwork.Status.NetworkName, clientContainer.Networks[0].Name, "Client proxy container should be attached to the target network during creation")
	require.Equal(t, aliases, clientContainer.Networks[0].Aliases, "Client proxy container should have the requested aliases on the target network")

	t.Log("Verifying client proxy container has correct labels...")
	require.Contains(t, clientContainer.Labels, controllers.CreatorProcessIdLabel, "Container should have creator process ID label")
	require.Equal(t, fmt.Sprintf("%d", os.Getpid()), clientContainer.Labels[controllers.CreatorProcessIdLabel], "Container should have correct creator process ID label")
	require.Contains(t, clientContainer.Labels, controllers.CreatorProcessStartTimeLabel, "Container should have creator process start time label")

	t.Log("Verifying server proxy status...")
	require.NotNil(t, updatedTunnelProxy.Status.ServerProxyProcessID, "Server proxy should have a process ID")
	require.False(t, updatedTunnelProxy.Status.ServerProxyStartupTimestamp.IsZero(), "Server proxy should have a startup timestamp")
	require.NotEmpty(t, updatedTunnelProxy.Status.ServerProxyStdOutFile, "Server proxy should publish a stdout log file path")
	require.NotEmpty(t, updatedTunnelProxy.Status.ServerProxyStdErrFile, "Server proxy should publish a stderr log file path")
	require.FileExists(t, updatedTunnelProxy.Status.ServerProxyStdOutFile)
	require.FileExists(t, updatedTunnelProxy.Status.ServerProxyStdErrFile)
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
	require.Contains(t, pe.Cmd.Args, "--server-control-address")
	serverAddrFlagIdx := std_slices.Index(pe.Cmd.Args, "--server-control-address")
	require.Greater(t, serverAddrFlagIdx, 0)
	require.Less(t, serverAddrFlagIdx+1, len(pe.Cmd.Args))
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, pe.Cmd.Args[serverAddrFlagIdx+1])
	require.Contains(t, pe.Cmd.Args, "--control-security-stdin")
	require.NotContains(t, pe.Cmd.Args, "--ca-cert")
	require.NotContains(t, pe.Cmd.Args, "--server-cert")
	require.NotContains(t, pe.Cmd.Args, "--server-key")
	require.NotNil(t, pe.Cmd.Stdin)
	serverSecurityBytes, serverSecurityReadErr := io.ReadAll(pe.Cmd.Stdin)
	require.NoError(t, serverSecurityReadErr)
	var serverSecurity dcptun.TunnelControlSecurityConfig
	require.NoError(t, json.Unmarshal(serverSecurityBytes, &serverSecurity))
	require.NoError(t, serverSecurity.ValidateServer())
	require.NoError(t, serverSecurity.ValidateClient())
	require.NotEqual(t, clientSecurity.ServerKeyBase64, serverSecurity.ServerKeyBase64)
	require.NotEqual(t, clientSecurity.ServerCertBase64, serverSecurity.ServerCertBase64)
	require.NotContains(t, string(clientSecurityBytes), serverSecurity.ServerKeyBase64)
	serverOutput, serverOutputErr := os.ReadFile(updatedTunnelProxy.Status.ServerProxyStdOutFile)
	require.NoError(t, serverOutputErr)
	serverErrorOutput, serverErrorReadErr := os.ReadFile(updatedTunnelProxy.Status.ServerProxyStdErrFile)
	require.NoError(t, serverErrorReadErr)
	physicalContainerJSON, physicalContainerMarshalErr := json.Marshal(clientPhysicalContainer)
	require.NoError(t, physicalContainerMarshalErr)
	proxyJSON, proxyMarshalErr := json.Marshal(updatedTunnelProxy)
	require.NoError(t, proxyMarshalErr)
	for _, privateValue := range []string{
		serverSecurity.PeerCACertBase64,
		serverSecurity.ServerCertBase64,
		serverSecurity.ServerKeyBase64,
		serverSecurity.OutgoingToken,
		clientSecurity.ServerCertBase64,
		clientSecurity.ServerKeyBase64,
	} {
		require.NotEmpty(t, privateValue)
		for _, publicOutput := range []string{
			strings.Join(pe.Cmd.Args, " "),
			string(physicalContainerJSON),
			string(proxyJSON),
			string(serverOutput),
			string(serverErrorOutput),
		} {
			require.NotContains(t, publicOutput, privateValue)
		}
	}

	sharedPhysicalImages := apiv2.PhysicalContainerImageList{}
	require.NoError(t, serverInfo.Client.List(ctx, &sharedPhysicalImages, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName)))
	require.Len(t, sharedPhysicalImages.Items, 1)
	require.Equal(t, controllers.V1TunnelProxyPhysicalContainerImageName, sharedPhysicalImages.Items[0].Name)
	require.Equal(t, 1, testContainerOrchestrator.BuildImageCallCount(updatedTunnelProxy.Status.ClientProxyContainerImage))
}

// Verifies that ContainerNetworkTunnelProxy retains private credentials during pending physical deletion
// and removes them with the proxy pair after deletion completes.
func TestTunnelProxyCleanup(t *testing.T) {
	tests := []struct {
		name               string
		testName           string
		serverExitedOnStop bool
	}{
		{"running server proxy", "test-tunnel-proxy-cleanup", false},
		{"server proxy already exited", "test-tunnel-proxy-cleanup-exited-server", true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			testTunnelProxyCleanup(t, testCase.testName, testCase.serverExitedOnStop)
		})
	}
}

func testTunnelProxyCleanup(t *testing.T, testName string, serverExitedOnStop bool) {
	t.Helper()
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	instanceTag := containertest.UniqueName(t, t.Name())
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, instanceTag, t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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
	var stopError func(*internal_testutil.ProcessExecution) error
	if serverExitedOnStop {
		stopError = func(execution *internal_testutil.ProcessExecution) error {
			execution.Signal <- syscall.SIGTERM
			execution.ExitCode = 0
			execution.EndedAt = time.Now()
			return fmt.Errorf("server proxy already exited: %w", &process.ErrProcessNotFound{Pid: execution.PID})
		}
	}
	simulateServerProxyWithLifecycleCallbacks(t, serverControlPort, teInfo.TestProcessExecutor, nil, stopError)

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

	physicalContainers := apiv2.PhysicalContainerList{}
	listContainersErr := serverInfo.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listContainersErr)
	require.Len(t, physicalContainers.Items, 1)
	physicalContainer := physicalContainers.Items[0]
	securityPath := physicalContainer.Spec.Container.CreateFiles[0].Entries[0].Source
	requireTunnelProxyCredentialPermissions(t, securityPath)
	const physicalDeletionHold = "test.dcp.microsoft.com/hold-credential-deletion"
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, physicalContainer.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
		current.Finalizers = append(current.Finalizers, physicalDeletionHold)
		return serverInfo.Client.Update(updateCtx, current)
	}))
	defer func() {
		releaseErr := retryOnConflictEx(ctx, serverInfo.Client, physicalContainer.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
			current.Finalizers = std_slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == physicalDeletionHold })
			return serverInfo.Client.Update(updateCtx, current)
		})
		if releaseErr != nil && !apimachinery_errors.IsNotFound(releaseErr) {
			t.Errorf("could not release physical-container deletion hold: %v", releaseErr)
		}
	}()

	physicalNetworks := apiv2.PhysicalContainerNetworkList{}
	listNetworksErr := serverInfo.Client.List(ctx, &physicalNetworks, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listNetworksErr)
	require.Len(t, physicalNetworks.Items, 1)
	physicalNetwork := physicalNetworks.Items[0]

	physicalConnections := apiv2.PhysicalContainerNetworkConnectionList{}
	listConnectionsErr := serverInfo.Client.List(ctx, &physicalConnections, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listConnectionsErr)
	require.Len(t, physicalConnections.Items, 1)
	physicalConnection := physicalConnections.Items[0]

	physicalImages := apiv2.PhysicalContainerImageList{}
	listImagesErr := serverInfo.Client.List(ctx, &physicalImages, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName))
	require.NoError(t, listImagesErr)
	require.Len(t, physicalImages.Items, 1)
	physicalImage := physicalImages.Items[0]
	require.NotNil(t, physicalImage.Spec.Image.Build)
	require.NotNil(t, physicalImage.Spec.Image.Build.ContextArchive)
	require.NotEmpty(t, physicalImage.Spec.Image.Build.ContextArchive.Source)
	require.Empty(t, physicalImage.Spec.Image.Build.ContextArchive.RawContents)
	require.FileExists(t, physicalImage.Spec.Image.Build.ContextArchive.Source)
	teInfo.addAfterShutdown(func() {
		require.NoFileExists(t, physicalImage.Spec.Image.Build.ContextArchive.Source)
	})

	t.Logf("Deleting ContainerNetworkTunnelProxy object '%s'", tunnelProxy.ObjectMeta.Name)
	err = retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(ctx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		return serverInfo.Client.Delete(ctx, tp)
	})
	require.NoError(t, err, "Could not delete ContainerNetworkTunnelProxy object")

	waitObjectAssumesStateEx(t, ctx, serverInfo.Client, physicalContainer.NamespacedName(), func(current *apiv2.PhysicalContainer) (bool, error) {
		return !current.DeletionTimestamp.IsZero(), nil
	})
	require.FileExists(t, securityPath, "An unfinished container operation may still need the source credentials")
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, physicalContainer.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
		current.Finalizers = std_slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == physicalDeletionHold })
		return serverInfo.Client.Update(updateCtx, current)
	}))

	t.Log("Waiting for controller to remove finalizer and complete deletion...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &tunnelProxy)

	t.Log("Verifying proxy resources are cleaned up...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &physicalContainer)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &physicalConnection)
	require.NoFileExists(t, securityPath)
	require.NoDirExists(t, filepath.Dir(securityPath))
	retainedPhysicalNetwork := apiv2.PhysicalContainerNetwork{}
	require.NoError(t, serverInfo.Client.Get(ctx, physicalNetwork.NamespacedName(), &retainedPhysicalNetwork))
	require.Equal(t, apiv2.PhysicalContainerNetworkPhaseReady, retainedPhysicalNetwork.Status.Phase)
	retainedPhysicalImage := apiv2.PhysicalContainerImage{}
	require.NoError(t, serverInfo.Client.Get(ctx, physicalImage.NamespacedName(), &retainedPhysicalImage))
	require.Equal(t, apiv2.PhysicalContainerImagePhaseReady, retainedPhysicalImage.Status.Phase)
	require.FileExists(t, retainedPhysicalImage.Spec.Image.Build.ContextArchive.Source)

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

// Verifies that ContainerNetworkTunnelProxy cleans abandoned credentials after delayed physical deletion
// while reconciliation is blocked, without removing the credentials of a recreated proxy with the same name.
func TestTunnelProxyAbandonedCredentialsSurvivePendingDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resumeReconciliation := make(chan struct{})
	releaseReconciliation := sync.OnceFunc(func() { close(resumeReconciliation) })
	defer releaseReconciliation()
	gate := &tunnelProxyReconciliationGate{entered: make(chan struct{}), resume: resumeReconciliation}
	server, environment, running := createRunningTunnelProxy(t, ctx, "abandoned-credentials", TestEnvironmentOptions{
		DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
			gate.Client = client
			return gate
		},
		DecorateTunnelProxyAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
			return &tunnelProxyReconciliationReaderGate{Reader: apiReader, gate: gate}
		},
	})
	oldPhysical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
		Namespace: controllers.V1PhysicalResourcesNamespaceName,
		Name:      fmt.Sprintf("tunnel-proxy-%s", running.UID),
	}, oldPhysical))
	oldSecurityPath := oldPhysical.Spec.Container.CreateFiles[0].Entries[0].Source
	const deletionHold = "test.dcp.microsoft.com/hold-abandoned-credentials"
	require.NoError(t, retryOnConflictEx(ctx, server.Client, oldPhysical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
		current.Finalizers = append(current.Finalizers, deletionHold)
		return server.Client.Update(updateCtx, current)
	}))
	defer func() {
		releaseErr := retryOnConflictEx(ctx, server.Client, oldPhysical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
			current.Finalizers = std_slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == deletionHold })
			return server.Client.Update(updateCtx, current)
		})
		if releaseErr != nil && !apimachinery_errors.IsNotFound(releaseErr) {
			t.Errorf("could not release abandoned physical-container deletion hold: %v", releaseErr)
		}
	}()
	require.NoError(t, server.Client.Delete(ctx, running))
	require.NoError(t, retryOnConflictEx(ctx, server.Client, running.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Finalizers = nil
		return server.Client.Update(updateCtx, current)
	}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, running)
	recreated := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: running.Name, Namespace: running.Namespace},
		Spec:       running.Spec,
	}
	require.NoError(t, server.Client.Create(ctx, recreated))
	require.NotEqual(t, running.UID, recreated.UID)
	waitObjectAssumesStateEx(t, ctx, server.Client, recreated.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
	newPhysical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
		Namespace: controllers.V1PhysicalResourcesNamespaceName,
		Name:      fmt.Sprintf("tunnel-proxy-%s", recreated.UID),
	}, newPhysical))
	newSecurityPath := newPhysical.Spec.Container.CreateFiles[0].Entries[0].Source
	require.NotEqual(t, oldSecurityPath, newSecurityPath)
	require.FileExists(t, oldSecurityPath)
	requireTunnelProxyCredentialPermissions(t, newSecurityPath)
	newSecurityBytes, readErr := os.ReadFile(newSecurityPath)
	require.NoError(t, readErr)
	gate.armed.Store(true)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, recreated.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		metav1.SetMetaDataAnnotation(&current.ObjectMeta, "test.dcp.microsoft.com/block-reconciliation", "true")
		return server.Client.Update(updateCtx, current)
	}))
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("Tunnel proxy reconciliation did not reach the API read gate")
	}
	require.NoError(t, retryOnConflictEx(ctx, server.Client, oldPhysical.NamespacedName(), func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
		current.Finalizers = std_slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == deletionHold })
		return server.Client.Update(updateCtx, current)
	}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, oldPhysical)
	cleanupErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		_, statErr := os.Stat(filepath.Dir(oldSecurityPath))
		if errors.Is(statErr, os.ErrNotExist) {
			return true, nil
		}
		return false, statErr
	})
	require.NoError(t, cleanupErr)
	currentSecurityBytes, currentReadErr := os.ReadFile(newSecurityPath)
	require.NoError(t, currentReadErr)
	require.True(t, bytes.Equal(newSecurityBytes, currentSecurityBytes), "Abandoned cleanup must not modify replacement credentials")
	environment.addAfterShutdown(func() {
		require.NoFileExists(t, newSecurityPath)
		require.NoDirExists(t, filepath.Dir(newSecurityPath))
	})
}

type tunnelProxyReconciliationGate struct {
	ctrl_client.Client
	armed                   atomic.Bool
	enterOnce               sync.Once
	entered                 chan struct{}
	resume                  <-chan struct{}
	physicalDeleteSubmitted *concurrency.ValuePromise[error]
}

func (gate *tunnelProxyReconciliationGate) blockIfArmedProxyGet(ctx context.Context, object ctrl_client.Object) error {
	if _, isProxy := object.(*apiv1.ContainerNetworkTunnelProxy); isProxy && gate.armed.Load() {
		gate.enterOnce.Do(func() { close(gate.entered) })
		select {
		case <-gate.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (gate *tunnelProxyReconciliationGate) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	if gateErr := gate.blockIfArmedProxyGet(ctx, object); gateErr != nil {
		return gateErr
	}
	return gate.Client.Get(ctx, key, object, options...)
}

type tunnelProxyReconciliationReaderGate struct {
	ctrl_client.Reader
	gate *tunnelProxyReconciliationGate
}

func (readerGate *tunnelProxyReconciliationReaderGate) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	if gateErr := readerGate.gate.blockIfArmedProxyGet(ctx, object); gateErr != nil {
		return gateErr
	}
	return readerGate.Reader.Get(ctx, key, object, options...)
}

func (gate *tunnelProxyReconciliationGate) Delete(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.DeleteOption,
) error {
	deleteErr := gate.Client.Delete(ctx, object, options...)
	if _, isPhysicalContainer := object.(*apiv2.PhysicalContainer); isPhysicalContainer && gate.physicalDeleteSubmitted != nil {
		gate.physicalDeleteSubmitted.Set(deleteErr)
	}
	return deleteErr
}

// Verifies that ContainerNetworkTunnelProxy cleans up a startup whose result cannot
// reach reconciliation before shutdown, without enqueueing a follow-up cleanup job.
func TestTunnelProxyCleansUpUnpublishedStartupOnShutdown(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	resume := make(chan struct{})
	releaseReconciliation := sync.OnceFunc(func() { close(resume) })
	defer releaseReconciliation()
	gate := &tunnelProxyReconciliationGate{
		entered:                 make(chan struct{}),
		resume:                  resume,
		physicalDeleteSubmitted: concurrency.NewValuePromise[error](),
	}
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, runCtx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir(), TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				gate.Client = client
				return gate
			},
			DecorateTunnelProxyAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
				return &tunnelProxyReconciliationReaderGate{Reader: apiReader, gate: gate}
			},
		})
	require.NoError(t, startupErr)
	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	serverConfigWritten := concurrency.NewValuePromise[bool]()
	readinessWritten := concurrency.NewValuePromise[bool]()
	serverStopped := concurrency.NewValuePromise[bool]()
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{dcpPath, "tunnel-server"},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			gate.armed.Store(true)
			tcBytes, tcErr := json.Marshal(dcptun.TunnelProxyConfig{ServerControlPort: 16785})
			if tcErr != nil {
				t.Errorf("Could not marshal TunnelProxyConfig: %v", tcErr)
				return 1
			}
			if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(tcBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyConfig to stdout: %v", writeErr)
				return 1
			}
			serverConfigWritten.Set(true)

			// Wait until the sole per-object Reconcile worker is pinned at the read gate
			// before reporting control readiness, ensuring startup cannot be published.
			select {
			case <-gate.entered:
			case <-ctx.Done():
				return 0
			}

			readinessBytes, readinessErr := json.Marshal(dcptun.TunnelProxyReadiness{ControlReady: true})
			if readinessErr != nil {
				t.Errorf("Could not marshal TunnelProxyReadiness: %v", readinessErr)
				return 1
			}
			if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(readinessBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyReadiness to stdout: %v", writeErr)
				return 1
			}
			readinessWritten.Set(true)

			select {
			case <-pe.Signal:
			case <-ctx.Done():
			}
			return 0
		},
		StopError: func(*internal_testutil.ProcessExecution) error {
			serverStopped.Set(true)
			return nil
		},
	})
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "unpublished-startup-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "unpublished-startup"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))
	select {
	case <-serverConfigWritten.Wait():
	case <-ctx.Done():
		t.Fatal("The server proxy did not write its configuration")
	}
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		metav1.SetMetaDataAnnotation(&current.ObjectMeta, "test.dcp.microsoft.com/block-startup-publication", "true")
		return server.Client.Update(updateCtx, current)
	}))
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("Startup did not reach blocked reconciliation")
	}
	select {
	case <-readinessWritten.Wait():
	case <-ctx.Done():
		t.Fatal("The server proxy did not write its readiness line")
	}
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
		Namespace: controllers.V1PhysicalResourcesNamespaceName,
		Name:      fmt.Sprintf("tunnel-proxy-%s", proxy.UID),
	}, physical))
	securityPath := physical.Spec.Container.CreateFiles[0].Entries[0].Source

	stopRun()
	select {
	case <-gate.physicalDeleteSubmitted.Wait():
	case <-ctx.Done():
		t.Fatal("Startup did not submit its owned container cleanup during shutdown")
	}
	require.NoError(t, gate.physicalDeleteSubmitted.Get())
	require.True(t, serverStopped.IsSet(), "Startup must stop its owned server before submitting container cleanup")
	select {
	case <-server.ApiServerDisposalComplete.Wait():
	case <-ctx.Done():
		t.Fatal("Unpublished startup prevented shutdown")
	}
	require.NoFileExists(t, securityPath)
	require.NoDirExists(t, filepath.Dir(securityPath))
}

// Verifies that ContainerNetworkTunnelProxy lets accepted cleanup finish during shutdown
// and cancels unfinished work at the drain deadline.
func TestTunnelProxyDrainsAbandonedCleanupOnShutdown(t *testing.T) {
	t.Parallel()
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout-%t", expire), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			runCtx, stopRun := context.WithCancel(ctx)
			defer stopRun()
			release := make(chan struct{})
			releaseCleanup := sync.OnceFunc(func() { close(release) })
			defer releaseCleanup()
			reader := &tunnelProxyCleanupFailureReader{
				entered:   concurrency.NewValuePromise[context.Context](),
				cancelled: concurrency.NewValuePromise[time.Time](),
				release:   release,
			}
			physicalDeleteSubmitted := concurrency.NewValuePromise[error]()
			deleteObserver := &tunnelProxyReconciliationGate{
				physicalDeleteSubmitted: physicalDeleteSubmitted,
			}
			drainTimeout := 20 * time.Second
			if expire {
				drainTimeout = 500 * time.Millisecond
			}
			server, _, running := createRunningTunnelProxy(t, runCtx, "abandoned-cleanup-shutdown", TestEnvironmentOptions{
				ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
					config.WorkQueueDrainTimeout = drainTimeout
				},
				DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
					deleteObserver.Client = client
					return deleteObserver
				},
				DecorateTunnelProxyAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
					reader.Reader = apiReader
					return reader
				},
			})
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, server.Client.Get(ctx, types.NamespacedName{
				Namespace: controllers.V1PhysicalResourcesNamespaceName,
				Name:      fmt.Sprintf("tunnel-proxy-%s", running.UID),
			}, physical))
			securityPath := physical.Spec.Container.CreateFiles[0].Entries[0].Source
			reader.armed.Store(true)
			require.NoError(t, server.Client.Delete(ctx, running))
			require.NoError(t, retryOnConflictEx(ctx, server.Client, running.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
				current.Finalizers = nil
				return server.Client.Update(updateCtx, current)
			}))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, running)
			select {
			case <-reader.entered.Wait():
			case <-ctx.Done():
				t.Fatal("Abandoned cleanup did not reach the API read gate")
			}
			require.FileExists(t, securityPath, "Unconfirmed container deletion must retain its credentials")

			shutdownStarted := time.Now()
			stopRun()
			if expire {
				select {
				case <-reader.cancelled.Wait():
				case <-ctx.Done():
					t.Fatal("The drain deadline did not cancel accepted cleanup")
				}
				elapsed := reader.cancelled.Get().Sub(shutdownStarted)
				require.GreaterOrEqual(t, elapsed, drainTimeout)
				require.Less(t, elapsed, 5*time.Second, "The queue deadline must cancel the API call before its own timeout")
			} else {
				require.NoError(t, reader.entered.Get().Err(), "Reconciler shutdown must not cancel accepted cleanup")
				releaseCleanup()
				select {
				case <-physicalDeleteSubmitted.Wait():
				case <-ctx.Done():
					t.Fatal("Released abandoned cleanup did not submit PhysicalContainer deletion during shutdown drain")
				}
				require.NoError(t, physicalDeleteSubmitted.Get())
			}
			select {
			case <-server.ApiServerDisposalComplete.Wait():
			case <-ctx.Done():
				t.Fatal("Unsuccessful abandoned cleanup prevented shutdown")
			}
			if !expire {
				require.False(t, reader.cancelled.IsSet(), "Accepted cleanup that finishes within drain timeout must not be cancelled")
			}
			require.NoFileExists(t, securityPath)
			require.NoDirExists(t, filepath.Dir(securityPath))
		})
	}
}

type tunnelProxyCleanupFailureReader struct {
	ctrl_client.Reader
	armed     atomic.Bool
	attempts  atomic.Uint32
	entered   *concurrency.ValuePromise[context.Context]
	cancelled *concurrency.ValuePromise[time.Time]
	release   <-chan struct{}
}

func (reader *tunnelProxyCleanupFailureReader) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	if _, isPhysicalContainer := object.(*apiv2.PhysicalContainer); isPhysicalContainer && reader.armed.Load() {
		if reader.attempts.Add(1) >= 3 {
			reader.entered.Set(ctx)
			select {
			case <-reader.release:
				reader.armed.Store(false)
				return reader.Reader.Get(ctx, key, object, options...)
			case <-ctx.Done():
				reader.cancelled.Set(time.Now())
				return ctx.Err()
			}
		}
		return errors.New("injected tunnel proxy cleanup API failure")
	}
	return reader.Reader.Get(ctx, key, object, options...)
}

// Verifies that a tunnel proxy can be created with a single tunnel, and that this tunnel gets successfully prepared.
func TestTunnelProxyTunnelCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-tunnel-management"

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

// Verifies that recovery retries the exact preparation request after a lost
// response, recovers the runtime ID, and then retargets a changed backend.
func TestTunnelProxyRecoveryRetriesLostPreparationRequest(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-lost-preparation-response"
	lostResponse := &lostTunnelPreparationResponse{
		firstApplied: make(chan *dcptunproto.TunnelSpec, 1),
		releaseFirst: make(chan struct{}),
		retryRequest: make(chan *dcptunproto.TunnelReq, 1),
	}
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController|ServiceController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					lostResponse.TunnelControlClient = makeClient(connection)
					return lostResponse
				}
			},
		},
	)
	require.NoError(t, startupErr)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	simulateServerProxy(t, 32967, teInfo.TestProcessExecutor)
	const originalServerPort int32 = 28272
	tunnelData := prepareTunnelServices(
		t,
		ctx,
		serverInfo.Client,
		teInfo.TestTunnelControlClient,
		1,
		testName,
		originalServerPort,
	)[0]
	tunnelProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{{
				Name: tunnelData.tunnelName, ServerServiceName: tunnelData.serverServiceName, ClientServiceName: tunnelData.clientServiceName,
			}},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, tunnelProxy))
	running := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	originalStatus := running.Status.TunnelStatuses[0]
	require.True(t, teInfo.TestTunnelControlClient.DisableTunnel(tunnelData.fingerprint))
	replacementSpec := &dcptunproto.TunnelSpec{
		ServerAddress:   stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:      stdproto.Int32(originalServerPort),
		ClientProxyPort: stdproto.Int32(originalStatus.ClientProxyPort),
	}
	replacementFingerprint := ctrl_testutil.TestFingerprint(replacementSpec)
	teInfo.TestTunnelControlClient.EnableTunnel(replacementSpec)
	lostResponse.loseNext.Store(true)

	serverPID, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	teInfo.TestProcessExecutor.SimulateProcessExit(t, serverPID, 0)

	var preparedWithoutResponse *dcptunproto.TunnelSpec
	select {
	case preparedWithoutResponse = <-lostResponse.firstApplied:
	case <-ctx.Done():
		t.Fatal("Replacement preparation did not reach the simulated lost response")
	}
	require.Equal(t, originalStatus.ClientProxyPort, preparedWithoutResponse.GetClientProxyPort())

	serverEndpointName := types.NamespacedName{Name: tunnelData.serverServiceEndpointName}
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, serverEndpointName, func(updateCtx context.Context, endpoint *apiv1.Endpoint) error {
		return serverInfo.Client.Delete(updateCtx, endpoint)
	}))
	const replacementServerPort int32 = originalServerPort + 1
	require.NoError(t, serverInfo.Client.Create(ctx, &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{Name: tunnelData.serverServiceEndpointName},
		Spec: apiv1.EndpointSpec{
			ServiceName: tunnelData.serverServiceName,
			Address:     networking.IPv4LocalhostDefaultAddress,
			Port:        replacementServerPort,
		},
	}))
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, types.NamespacedName{Name: tunnelData.serverServiceName}, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateReady && svc.Status.EffectivePort == replacementServerPort, nil
	})
	close(lostResponse.releaseFirst)

	var retryRequest *dcptunproto.TunnelReq
	select {
	case retryRequest = <-lostResponse.retryRequest:
	case <-ctx.Done():
		t.Fatal("Controller did not retry the lost preparation request")
	}
	require.Equal(t, originalServerPort, retryRequest.GetServerPort())
	require.Equal(t, originalStatus.ClientProxyPort, retryRequest.GetClientProxyPort())

	recovered := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	recoveredStatus := recovered.Status.TunnelStatuses[0]
	require.Equal(t, preparedWithoutResponse.GetTunnelRef().GetTunnelId(), recoveredStatus.TunnelID)
	require.Equal(t, originalStatus.ClientProxyPort, recoveredStatus.ClientProxyPort)
	require.NotEqual(t, *running.Status.ServerProxyProcessID, *recovered.Status.ServerProxyProcessID)

	retargetedFingerprint := replacementFingerprint
	retargetedFingerprint.ServerPort = replacementServerPort
	var retargetedSpec *dcptunproto.TunnelSpec
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		retargetedSpec = teInfo.TestTunnelControlClient.GetTunnelSpec(retargetedFingerprint)
		return retargetedSpec != nil, nil
	}))
	require.Equal(t, recoveredStatus.TunnelID, retargetedSpec.GetTunnelRef().GetTunnelId())
	require.Equal(t, replacementServerPort, retargetedSpec.GetServerPort())
}

// Verifies that the ContainerNetworkTunnelProxy controller retries a new tunnel's preferred-port preparation
// request unchanged after its response is lost, adopts the tunnel prepared on the preferred port,
// and does not prepare a second, random-port tunnel for the same configuration.
func TestTunnelProxyLostPreferredPortResponseDoesNotOrphanTunnel(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-lost-preferred-port-response"
	lostResponse := &lostTunnelPreparationResponse{
		firstApplied: make(chan *dcptunproto.TunnelSpec, 1),
		releaseFirst: make(chan struct{}),
		retryRequest: make(chan *dcptunproto.TunnelReq, 1),
	}
	lostResponse.loseNext.Store(true)
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController|ServiceController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					lostResponse.TunnelControlClient = makeClient(connection)
					return lostResponse
				}
			},
		},
	)
	require.NoError(t, startupErr)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	simulateServerProxy(t, 32968, teInfo.TestProcessExecutor)
	const serverServicePort int32 = 28273
	tunnelData := prepareTunnelServices(
		t,
		ctx,
		serverInfo.Client,
		teInfo.TestTunnelControlClient,
		1,
		testName,
		serverServicePort,
	)[0]
	randomPortSpec := &dcptunproto.TunnelSpec{
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(serverServicePort),
	}
	teInfo.TestTunnelControlClient.EnableTunnel(randomPortSpec)

	tunnelProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{{
				Name: tunnelData.tunnelName, ServerServiceName: tunnelData.serverServiceName, ClientServiceName: tunnelData.clientServiceName,
			}},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, tunnelProxy))

	var preparedWithoutResponse *dcptunproto.TunnelSpec
	select {
	case preparedWithoutResponse = <-lostResponse.firstApplied:
	case <-ctx.Done():
		t.Fatal("Tunnel preparation did not reach the simulated lost response")
	}
	require.Equal(t, serverServicePort, preparedWithoutResponse.GetClientProxyPort())
	close(lostResponse.releaseFirst)

	var retryRequest *dcptunproto.TunnelReq
	select {
	case retryRequest = <-lostResponse.retryRequest:
	case <-ctx.Done():
		t.Fatal("Controller did not retry the lost preparation request")
	}
	require.Equal(t, serverServicePort, retryRequest.GetServerPort())
	require.Equal(t, serverServicePort, retryRequest.GetClientProxyPort(), "The retry must keep the preferred port")
	require.True(t, retryRequest.GetAllowClientProxyPortFallback(), "The retry must keep allowing random port fallback")

	running := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	tunnelStatus := running.Status.TunnelStatuses[0]
	require.Equal(t, preparedWithoutResponse.GetTunnelRef().GetTunnelId(), tunnelStatus.TunnelID)
	require.Equal(t, serverServicePort, tunnelStatus.ClientProxyPort)
	validateTunnel(t, ctx, tunnelStatus, tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)

	randomPortTunnel := teInfo.TestTunnelControlClient.GetTunnelSpec(ctrl_testutil.TestFingerprint(randomPortSpec))
	require.NotNil(t, randomPortTunnel)
	require.Equal(t, ctrl_testutil.InvalidTunnelID, randomPortTunnel.GetTunnelRef().GetTunnelId(), "No random-port tunnel should be prepared")
}

// Verifies that a tunnel uses a random port when its preferred port is unavailable,
// retries that exact published port without a retry limit after pair recreation,
// and excludes it from newly added tunnels that need random fallback.
func TestTunnelProxyTunnelCreateFallsBackToRandomPort(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-tunnel-port-fallback"

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	var publishedPort atomic.Int32
	var blockPublishedPort atomic.Bool
	var rebindAttempts atomic.Uint32
	var addedTunnelAttempts atomic.Uint32
	var addedFallbackExcludedPublishedPort atomic.Bool
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx, includedControllers, t.Name(), t.TempDir(), TestEnvironmentOptions{
		ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
			makeClient := config.MakeTunnelControlClient
			config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
				return &tunnelPreparationObserver{
					TunnelControlClient: makeClient(connection),
					beforePrepare: func(request *dcptunproto.TunnelReq) error {
						port := publishedPort.Load()
						if port == 0 {
							return nil
						}
						if request.GetAllowClientProxyPortFallback() {
							if request.GetServerPort() == port {
								addedTunnelAttempts.Add(1)
								if std_slices.Contains(request.GetExcludedClientProxyPorts(), port) {
									addedFallbackExcludedPublishedPort.Store(true)
								}
							}
							return nil
						}
						if request.GetClientProxyPort() == port && blockPublishedPort.Load() {
							rebindAttempts.Add(1)
							return status.Error(codes.Unavailable, "published port is temporarily unavailable")
						}
						return nil
					},
				}
			}
		},
	})
	require.NoError(t, startupErr, "Failed to start the API server")

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 32966
	simulateServerProxy(t, serverControlPort, teInfo.TestProcessExecutor)

	const tunnelCount = 1
	const serverServicePort int32 = 28271
	ts := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, tunnelCount, testName, serverServicePort)
	tunnelData := ts[0]

	disabled := teInfo.TestTunnelControlClient.DisableTunnel(tunnelData.fingerprint)
	require.True(t, disabled, "Expected preferred-port tunnel to be disabled successfully")

	fallbackTunnelSpec := &dcptunproto.TunnelSpec{
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(serverServicePort),
	}
	fallbackFingerprint := ctrl_testutil.TestFingerprint(fallbackTunnelSpec)
	teInfo.TestTunnelControlClient.EnableTunnel(fallbackTunnelSpec)
	tunnelData.fingerprint = fallbackFingerprint

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
	require.NotEqual(t, serverServicePort, updatedProxy.Status.TunnelStatuses[0].ClientProxyPort, "Tunnel should fall back to a random port")

	validateTunnel(t, ctx, updatedProxy.Status.TunnelStatuses[0], tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)

	originalPort := updatedProxy.Status.TunnelStatuses[0].ClientProxyPort
	publishedPort.Store(originalPort)
	blockPublishedPort.Store(true)
	originalContainerID := updatedProxy.Status.ClientProxyContainerID
	originalServerPID := *updatedProxy.Status.ServerProxyProcessID
	require.True(t, teInfo.TestTunnelControlClient.DisableTunnel(fallbackFingerprint))
	pinnedSpec := &dcptunproto.TunnelSpec{
		ServerAddress:   stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:      stdproto.Int32(serverServicePort),
		ClientProxyPort: stdproto.Int32(originalPort),
	}
	teInfo.TestTunnelControlClient.EnableTunnel(pinnedSpec)
	tunnelData.fingerprint = ctrl_testutil.TestFingerprint(pinnedSpec)
	serverPID, pidErr := process.Int64_ToPidT(originalServerPID)
	require.NoError(t, pidErr)
	teInfo.TestProcessExecutor.SimulateProcessExit(t, serverPID, 0)

	restarting := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			tp.Status.ServerProxyProcessID != nil &&
			*tp.Status.ServerProxyProcessID != originalServerPID &&
			tp.Status.ClientProxyContainerID != originalContainerID, nil
	})
	unrelatedPort := int32(21001)
	if unrelatedPort == originalPort || unrelatedPort == serverServicePort {
		unrelatedPort++
	}
	unrelatedTunnel := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName+"-unrelated", unrelatedPort)[0]
	addedTunnel := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName+"-added", originalPort)[0]
	require.True(t, teInfo.TestTunnelControlClient.DisableTunnel(addedTunnel.fingerprint))
	addedFallback := &dcptunproto.TunnelSpec{
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(originalPort),
	}
	teInfo.TestTunnelControlClient.EnableTunnel(addedFallback)
	addedTunnel.fingerprint = ctrl_testutil.TestFingerprint(addedFallback)
	restarting.Spec.Tunnels = append(
		restarting.Spec.Tunnels,
		apiv1.TunnelConfiguration{
			Name:              unrelatedTunnel.tunnelName,
			ServerServiceName: unrelatedTunnel.serverServiceName,
			ClientServiceName: unrelatedTunnel.clientServiceName,
		},
		apiv1.TunnelConfiguration{
			Name:              addedTunnel.tunnelName,
			ServerServiceName: addedTunnel.serverServiceName,
			ClientServiceName: addedTunnel.clientServiceName,
		},
	)
	require.NoError(t, serverInfo.Client.Update(ctx, restarting))
	retryErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
		return rebindAttempts.Load() > 2, nil
	})
	require.NoError(t, retryErr, "Published ports must retry beyond the initial preparation-attempt limit")
	progressing := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		statuses := maps.SliceToMap(tp.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
		unrelatedStatus, found := statuses[unrelatedTunnel.tunnelName]
		return found && unrelatedStatus.State == apiv1.TunnelStateReady, nil
	})
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, progressing.Status.State)
	progressingStatuses := maps.SliceToMap(progressing.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Equal(t, originalPort, progressingStatuses[tunnelData.tunnelName].ClientProxyPort)
	require.Contains(t, progressingStatuses[tunnelData.tunnelName].ErrorMessage, "Failed to rebind published tunnel port")
	require.Positive(t, addedTunnelAttempts.Load(), "An unrelated tunnel should proceed to random fallback while the retained listener is unavailable")
	require.True(t, addedFallbackExcludedPublishedPort.Load(), "Random fallback must exclude the retained published port")
	blockPublishedPort.Store(false)
	recovered := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), 3, apiv1.TunnelStateReady)
	require.Equal(t, updatedProxy.UID, recovered.UID)
	statuses := maps.SliceToMap(recovered.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Equal(t, originalPort, statuses[tunnelData.tunnelName].ClientProxyPort)
	require.Empty(t, statuses[tunnelData.tunnelName].ErrorMessage)
	require.Greater(t, recovered.Status.TunnelConfigurationVersion, updatedProxy.Status.TunnelConfigurationVersion)
	validateTunnel(t, ctx, statuses[tunnelData.tunnelName], tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)
	validateTunnel(t, ctx, statuses[unrelatedTunnel.tunnelName], unrelatedTunnel, serverInfo.Client, teInfo.TestTunnelControlClient)
	validateTunnel(t, ctx, statuses[addedTunnel.tunnelName], addedTunnel, serverInfo.Client, teInfo.TestTunnelControlClient)
}

type tunnelPreparationObserver struct {
	dcptunproto.TunnelControlClient
	beforePrepare func(*dcptunproto.TunnelReq) error
}

func (observer *tunnelPreparationObserver) PrepareTunnel(ctx context.Context, request *dcptunproto.TunnelReq, options ...grpc.CallOption) (*dcptunproto.TunnelSpec, error) {
	if observeErr := observer.beforePrepare(request); observeErr != nil {
		return nil, observeErr
	}
	return observer.TunnelControlClient.PrepareTunnel(ctx, request, options...)
}

type tunnelDeletionObserver struct {
	dcptunproto.TunnelControlClient
	blocked  *atomic.Bool
	attempts *atomic.Uint32
}

func (observer *tunnelDeletionObserver) DeleteTunnel(
	ctx context.Context,
	request *dcptunproto.TunnelRef,
	options ...grpc.CallOption,
) (*emptypb.Empty, error) {
	observer.attempts.Add(1)
	if observer.blocked.Load() {
		return nil, status.Error(codes.Unavailable, "tunnel deletion is temporarily blocked")
	}
	return observer.TunnelControlClient.DeleteTunnel(ctx, request, options...)
}

// Verifies that when tunnel preparation fails after maximum attempts, the tunnel status indicates failure.
func TestTunnelProxyTunnelFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-tunnel-failure"

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

// Verifies that every controller-created gRPC client connection is closed after
// a failed tunnel preparation attempt.
func TestTunnelProxyClosesControlConnectionsAfterPreparation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-closes-control-connections"

	var connectionsLock sync.Mutex
	var connections []*grpc.ClientConn
	var connectionTypeError string
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController|ServiceController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					clientConnection, isClientConnection := connection.(*grpc.ClientConn)
					if !isClientConnection {
						connectionsLock.Lock()
						connectionTypeError = fmt.Sprintf("unexpected tunnel control connection type %T", connection)
						connectionsLock.Unlock()
						return makeClient(connection)
					}
					connectionsLock.Lock()
					connections = append(connections, clientConnection)
					connectionsLock.Unlock()
					return makeClient(connection)
				}
			},
		},
	)
	require.NoError(t, startupErr)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	simulateServerProxy(t, 33457, teInfo.TestProcessExecutor)
	tunnelData := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName, 29382)[0]
	require.True(t, teInfo.TestTunnelControlClient.DisableTunnel(tunnelData.fingerprint))
	tunnelProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{{
				Name: tunnelData.tunnelName, ServerServiceName: tunnelData.serverServiceName, ClientServiceName: tunnelData.clientServiceName,
			}},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, tunnelProxy))
	_ = waitForTunnelFailure(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelData.tunnelName)

	connectionsErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
		connectionsLock.Lock()
		defer connectionsLock.Unlock()
		if connectionTypeError != "" {
			return false, errors.New(connectionTypeError)
		}
		if len(connections) < 2 {
			return false, nil
		}
		for _, connection := range connections {
			if connection.GetState() != connectivity.Shutdown {
				return false, nil
			}
		}
		return true, nil
	})
	require.NoError(t, connectionsErr)
}

// Verifies that server Service loss suspends a tunnel without changing its
// client identity and that a changed backend resumes the same listener.
func TestTunnelProxyServerServiceTransition(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-server-service-transition"

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	var deletionAttempts atomic.Uint32
	endpointFaults := &tunnelEndpointFaultClient{
		clientServiceName: testName + "-client-service-0",
	}
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		includedControllers,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				endpointFaults.Client = client
				return endpointFaults
			},
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					return &tunnelDeletionObserver{
						TunnelControlClient: makeClient(connection),
						blocked:             &atomic.Bool{},
						attempts:            &deletionAttempts,
					}
				}
			},
		},
	)
	require.NoError(t, startupErr, "Failed to start the API server")

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
	originalStatus := updatedProxy.Status.TunnelStatuses[0]

	t.Logf("Validating tunnel '%s' is initially prepared and live...", tunnelData.tunnelName)
	validateTunnel(t, ctx, originalStatus, tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)

	t.Logf("Deleting server Service Endpoint '%s' to trigger Service transition to NotReady state...", tunnelData.serverServiceEndpointName)
	endpointFaults.failNextEndpointDelete.Store(true)
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

	t.Logf("Waiting for tunnel '%s' to suspend while retaining its listener identity...", tunnelData.tunnelName)
	suspendedProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return len(tp.Status.TunnelStatuses) == 1 &&
			tp.Status.TunnelStatuses[0].State == apiv1.TunnelStateNotReady &&
			tp.Status.TunnelStatuses[0].ErrorMessage != "", nil
	})
	suspendedStatus := suspendedProxy.Status.TunnelStatuses[0]
	require.Equal(t, originalStatus.TunnelID, suspendedStatus.TunnelID)
	require.Equal(t, originalStatus.ClientProxyAddresses, suspendedStatus.ClientProxyAddresses)
	require.Equal(t, originalStatus.ClientProxyPort, suspendedStatus.ClientProxyPort)
	require.Contains(t, suspendedStatus.ErrorMessage, "client listener remains reserved")
	waitEndpointCountEx(t, ctx, serverInfo.Client, tunnelData.clientServiceName, 0)
	require.GreaterOrEqual(t, endpointFaults.endpointDeleteAttempts.Load(), uint32(2))
	require.Zero(t, deletionAttempts.Load(), "Backend loss must suspend the tunnel instead of deleting it")
	accepting, found := teInfo.TestTunnelControlClient.TunnelAccepting(originalStatus.TunnelID)
	require.True(t, found)
	require.False(t, accepting)

	t.Logf("Re-creating server Service Endpoint '%s' to trigger Service transition back to Ready...", tunnelData.serverServiceEndpointName)
	restoredServerServicePort := serverServicePort + 1
	serverEndpoint := apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tunnelData.serverServiceEndpointName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceName:      tunnelData.serverServiceName,
			ServiceNamespace: metav1.NamespaceNone,
			Address:          networking.IPv4LocalhostDefaultAddress,
			Port:             restoredServerServicePort,
		},
	}
	err = serverInfo.Client.Create(ctx, &serverEndpoint)
	require.NoError(t, err, "Could not re-create server Service Endpoint '%s'", tunnelData.serverServiceEndpointName)

	t.Logf("Waiting for server Service '%s' to transition back to Ready state...", tunnelData.serverServiceName)
	_ = waitServiceReadyEx(t, ctx, serverInfo.Client, serverServiceNamespacedName)

	t.Logf("Waiting for tunnel '%s' to be prepared again...", tunnelData.tunnelName)
	restoredProxy := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), tunnelCount, apiv1.TunnelStateReady)
	restoredStatus := restoredProxy.Status.TunnelStatuses[0]
	require.Equal(t, originalStatus.TunnelID, restoredStatus.TunnelID)
	require.Equal(t, originalStatus.ClientProxyAddresses, restoredStatus.ClientProxyAddresses)
	require.Equal(t, originalStatus.ClientProxyPort, restoredStatus.ClientProxyPort)
	require.Zero(t, deletionAttempts.Load(), "Resuming a suspended tunnel must not delete it")
	accepting, found = teInfo.TestTunnelControlClient.TunnelAccepting(originalStatus.TunnelID)
	require.True(t, found)
	require.True(t, accepting)
	waitEndpointCountEx(t, ctx, serverInfo.Client, tunnelData.clientServiceName, 1)

	t.Logf("Validating tunnel '%s' is restored and live again...", tunnelData.tunnelName)
	tunnelData.fingerprint.ServerPort = restoredServerServicePort
	validateTunnel(t, ctx, restoredStatus, tunnelData, serverInfo.Client, teInfo.TestTunnelControlClient)
}

// Verifies that ContainerNetworkTunnelProxy suspends a prepared tunnel and withdraws its Endpoints when the
// backend loses its address after the client Service was deleted, instead of leaving the tunnel Ready.
func TestTunnelProxySuspendsTunnelWithoutClientService(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "suspends-without-client-service"

	server, environment, startupErr := StartTestEnvironment(t, ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	simulateServerProxy(t, 16806, environment.TestProcessExecutor)
	tunnel := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 1, testName, 31120)[0]
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{{
				Name: tunnel.tunnelName, ServerServiceName: tunnel.serverServiceName, ClientServiceName: tunnel.clientServiceName,
			}},
		},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))
	ready := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	readyStatus := ready.Status.TunnelStatuses[0]
	waitEndpointCountEx(t, ctx, server.Client, tunnel.clientServiceName, 1)

	clientService := &apiv1.Service{}
	require.NoError(t, server.Client.Get(ctx, tunnel.clientServiceNamespacedName, clientService))
	require.NoError(t, server.Client.Delete(ctx, clientService))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, clientService)

	serverEndpoint := &apiv1.Endpoint{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Name: tunnel.serverServiceEndpointName}, serverEndpoint))
	require.NoError(t, server.Client.Delete(ctx, serverEndpoint))
	_ = waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Name: tunnel.serverServiceName}, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.EffectiveAddress == "" && svc.Status.EffectivePort == 0, nil
	})

	suspended := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv1.TunnelStateNotReady, nil
	})
	suspendedStatus := suspended.Status.TunnelStatuses[0]
	require.Equal(t, readyStatus.TunnelID, suspendedStatus.TunnelID)
	require.Equal(t, readyStatus.ClientProxyPort, suspendedStatus.ClientProxyPort)
	require.Contains(t, suspendedStatus.ErrorMessage, "client listener remains reserved")
	accepting, found := environment.TestTunnelControlClient.TunnelAccepting(readyStatus.TunnelID)
	require.True(t, found)
	require.False(t, accepting, "A tunnel without a backend address must not accept new streams")
	waitEndpointCountEx(t, ctx, server.Client, tunnel.clientServiceName, 0)
}

// Verifies that multiple tunnels can be created and deleted for a single ContainerNetworkTunnelProxy object.
func TestTunnelProxyMultipleTunnels(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-multiple-tunnels"

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController | ServiceController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

// Verifies that removing a tunnel from the spec publishes Deleting, withdraws
// endpoints, and retries the control operation before dropping its bookkeeping.
func TestTunnelProxyRetriesSpecRemovalDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-retries-spec-removal"

	var deletionBlocked atomic.Bool
	var deletionAttempts atomic.Uint32
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController|ServiceController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					return &tunnelDeletionObserver{
						TunnelControlClient: makeClient(connection),
						blocked:             &deletionBlocked,
						attempts:            &deletionAttempts,
					}
				}
			},
		},
	)
	require.NoError(t, startupErr)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	simulateServerProxy(t, 45891, teInfo.TestProcessExecutor)
	tunnelData := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName, 40120)[0]
	tunnelProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{{
				Name: tunnelData.tunnelName, ServerServiceName: tunnelData.serverServiceName, ClientServiceName: tunnelData.clientServiceName,
			}},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, tunnelProxy))
	running := waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	originalStatus := running.Status.TunnelStatuses[0]

	deletionBlocked.Store(true)
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = nil
		return serverInfo.Client.Update(updateCtx, current)
	}))
	deleting := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return len(current.Status.TunnelStatuses) == 1 &&
			current.Status.TunnelStatuses[0].State == apiv1.TunnelStateDeleting &&
			current.Status.TunnelStatuses[0].ErrorMessage != "", nil
	})
	require.Equal(t, originalStatus.TunnelID, deleting.Status.TunnelStatuses[0].TunnelID)
	require.Equal(t, originalStatus.ClientProxyPort, deleting.Status.TunnelStatuses[0].ClientProxyPort)
	waitEndpointCountEx(t, ctx, serverInfo.Client, tunnelData.clientServiceName, 0)
	retryErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
		return deletionAttempts.Load() >= 2, nil
	})
	require.NoError(t, retryErr)

	deletionBlocked.Store(false)
	validateTunnelDeleted(t, ctx, serverInfo.Client, tunnelData, teInfo.TestTunnelControlClient)
	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), 0, apiv1.TunnelStateReady)
}

// Verifies that client-side proxy container is created with specified aliases when requested.
func TestTunnelProxyClientProxyAliases(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-client-proxy-aliases"

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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
// Verifies that failed server startup removes the client container and attempt-owned output files,
// and recreating the resource with a new UID allows a fresh startup.
func TestTunnelProxyServerStartupFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-server-startup-failure"

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr, "Could not get DCP executable path")

	t.Log("Installing server proxy auto execution with startup error...")
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}}
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		StartupError: func(*internal_testutil.ProcessExecution) error {
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
	failedProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})
	failedExecutions := teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil)
	require.Len(t, failedExecutions, 1)
	failedExecution := failedExecutions[0]
	stdoutFile, stdoutOK := failedExecution.Cmd.Stdout.(*os.File)
	stderrFile, stderrOK := failedExecution.Cmd.Stderr.(*os.File)
	require.True(t, stdoutOK)
	require.True(t, stderrOK)
	require.NoFileExists(t, stdoutFile.Name())
	require.NoFileExists(t, stderrFile.Name())

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true /* poll immediately */, func(_ context.Context) (bool, error) {
		return containerStarted.Load() == 1 && containerRemoved.Load() == 1, nil
	})
	require.NoError(t, waitErr, "Expected client proxy container to have been started and then removed")

	require.NoError(t, serverInfo.Client.Delete(ctx, failedProxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, failedProxy)
	simulateServerProxy(t, 15679, teInfo.TestProcessExecutor)
	recreatedProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: tunnelProxy.Name},
		Spec:       tunnelProxy.Spec,
	}
	require.NoError(t, serverInfo.Client.Create(ctx, recreatedProxy))
	require.NotEqual(t, failedProxy.UID, recreatedProxy.UID)
	runningProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, recreatedProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
	require.Empty(t, runningProxy.Status.Message)
	require.FileExists(t, runningProxy.Status.ServerProxyStdOutFile)
	require.FileExists(t, runningProxy.Status.ServerProxyStdErrFile)
	require.Len(t, teInfo.TestProcessExecutor.FindAll([]string{dcpPath, "tunnel-server"}, "", nil), 2)
}

// Verifies that proxy pairs which never establish their first control stream
// exhaust the configured generation budget and settle in Failed.
func TestTunnelProxyNeverReadyGenerationBudget(t *testing.T) {
	t.Parallel()
	testTunnelProxyNeverReadyGenerationBudget(
		t,
		"test-tunnel-proxy-never-ready-budget",
		100*time.Millisecond,
		"",
		func(ctx context.Context, execution *internal_testutil.ProcessExecution) int32 {
			select {
			case <-execution.Signal:
			case <-ctx.Done():
			}
			return 0
		},
	)
}

// Verifies that ContainerNetworkTunnelProxyReconciler counts server proxy exits before control readiness
// as never-ready generations, stops waiting for readiness once the server proxy exits,
// and settles in Failed after the generation budget is exhausted.
func TestTunnelProxyNeverReadyBudgetCountsServerExits(t *testing.T) {
	t.Parallel()
	// The startup timeout outlasts the test context, so only server exits can end the readiness waits.
	testTunnelProxyNeverReadyGenerationBudget(
		t,
		"test-tunnel-proxy-never-ready-server-exits",
		10*time.Minute,
		"exited with exit code 1",
		func(_ context.Context, _ *internal_testutil.ProcessExecution) int32 {
			return 1
		},
	)
}

func testTunnelProxyNeverReadyGenerationBudget(
	t *testing.T,
	testName string,
	controlStartupTimeout time.Duration,
	expectedMessage string,
	runServerAfterConfiguration func(context.Context, *internal_testutil.ProcessExecution) int32,
) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				config.ControlStartupTimeout = controlStartupTimeout
				config.MaxNeverReadyGenerations = 4
				config.PairRestartBackoffFactory = func() backoff.BackOff {
					return backoff.NewConstantBackOff(time.Millisecond)
				}
			},
		},
	)
	require.NoError(t, startupErr)
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}}
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			configBytes, configErr := json.Marshal(dcptun.TunnelProxyConfig{ServerControlPort: 16790})
			if configErr != nil {
				t.Errorf("Could not marshal tunnel server configuration: %v", configErr)
				return 1
			}
			if _, writeErr := execution.Cmd.Stdout.Write(osutil.WithNewline(configBytes)); writeErr != nil {
				t.Errorf("Could not write tunnel server configuration: %v", writeErr)
				return 1
			}
			return runServerAfterConfiguration(ctx, execution)
		},
	})

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))
	isBudgetExhausted := func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed &&
			strings.Contains(current.Status.Message, "generation 4 of 4") &&
			strings.Contains(current.Status.Message, expectedMessage), nil
	}
	failed := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), isBudgetExhausted)
	require.Len(t, teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil), 4)

	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, failed.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Status.Message = "force terminal reconciliation"
		return serverInfo.Client.Status().Update(updateCtx, current)
	}))
	settled := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, failed.NamespacedName(), isBudgetExhausted)
	require.Equal(t, failed.UID, settled.UID)
	require.Len(t, teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil), 4)
}

// Verifies that ContainerNetworkTunnelProxyReconciler retries a replacement pair whose server proxy exits
// before establishing its control stream, so recovery reaches Running instead of failing the proxy.
func TestTunnelProxyRecoveryRetriesReplacementThatExitsBeforeReady(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-replacement-exits-before-ready"
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				// The startup timeout outlasts the test context, so only the server exit can end the replacement's readiness wait.
				config.ControlStartupTimeout = 10 * time.Minute
				config.PairRestartBackoffFactory = func() backoff.BackOff {
					return backoff.NewConstantBackOff(time.Millisecond)
				}
			},
		},
	)
	require.NoError(t, startupErr)
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}}
	var serverStarts atomic.Int32
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			exitBeforeReady := serverStarts.Add(1) == 2
			outputLines := []any{dcptun.TunnelProxyConfig{ServerControlPort: 16792}}
			if !exitBeforeReady {
				outputLines = append(outputLines, dcptun.TunnelProxyReadiness{ControlReady: true})
			}
			for _, outputLine := range outputLines {
				lineBytes, marshalErr := json.Marshal(outputLine)
				if marshalErr != nil {
					t.Errorf("Could not marshal tunnel server output: %v", marshalErr)
					return 1
				}
				if _, writeErr := execution.Cmd.Stdout.Write(osutil.WithNewline(lineBytes)); writeErr != nil {
					t.Errorf("Could not write tunnel server output: %v", writeErr)
					return 1
				}
			}
			if exitBeforeReady {
				return 1
			}
			select {
			case <-execution.Signal:
			case <-ctx.Done():
			}
			return 0
		},
	})

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			current.Status.ServerProxyProcessID != nil &&
			current.Status.ClientProxyContainerID != "", nil
	})
	initialServerPID := *running.Status.ServerProxyProcessID
	initialContainerID := running.Status.ClientProxyContainerID
	serverPID, pidErr := process.Int64_ToPidT(initialServerPID)
	require.NoError(t, pidErr)

	t.Log("Simulating loss of the initial server proxy; the first replacement server will exit before it is ready...")
	teInfo.TestProcessExecutor.SimulateProcessExit(t, serverPID, 0)
	recovered := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		if current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed {
			return false, fmt.Errorf("tunnel proxy failed instead of retrying the replacement pair: %s", current.Status.Message)
		}
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			current.Status.ServerProxyProcessID != nil &&
			*current.Status.ServerProxyProcessID != initialServerPID &&
			current.Status.ClientProxyContainerID != "" &&
			current.Status.ClientProxyContainerID != initialContainerID, nil
	})
	require.Equal(t, proxy.UID, recovered.UID)
	require.Len(t, teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil), 3)
}

// Verifies that configuration failure retains an unconfirmed server identity until cleanup
// succeeds, preserves the original failure across repeated reconciliation, and never relaunches the server.
func TestTunnelProxyConfigurationFailureCleansUpWithoutRelaunching(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, teInfo, environmentErr := StartTestEnvironment(
		t, ctx, ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir(),
	)
	require.NoError(t, environmentErr)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "config-failure-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}}
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			if _, writeErr := execution.Cmd.Stdout.Write([]byte("invalid configuration\n")); writeErr != nil {
				t.Errorf("could not write invalid server configuration: %v", writeErr)
				return 1
			}
			select {
			case _, open := <-execution.Signal:
				if !open {
					t.Error("server signal channel closed unexpectedly")
				}
			case <-ctx.Done():
			}
			return 0
		},
		StopError: func(*internal_testutil.ProcessExecution) error {
			return errors.New("server cleanup could not be confirmed")
		},
	})
	t.Cleanup(func() { teInfo.TestProcessExecutor.RemoveAutoExecution(criteria) })
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "config-failure-proxy"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))
	failedProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed && current.Status.ServerProxyProcessID != nil, nil
	})
	pid, pidErr := process.Int64_ToPidT(*failedProxy.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	execution, executionFound := teInfo.TestProcessExecutor.FindByPid(pid)
	require.True(t, executionFound)
	handle := process.NewHandle(pid, execution.StartedAt)
	require.Equal(t, int64(handle.Pid), *failedProxy.Status.ServerProxyProcessID)
	require.WithinDuration(t, handle.IdentityTime, failedProxy.Status.ServerProxyStartupTimestamp.Time, time.Microsecond)
	require.Contains(t, failedProxy.Status.Message, "Failed to read server proxy configuration:")
	require.NoError(t, teInfo.TestProcessExecutor.CheckProcessRunning(handle))
	simulateServerProxy(t, 15680, teInfo.TestProcessExecutor)
	completedProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed && current.Status.ServerProxyProcessID == nil, nil
	})
	require.Contains(t, completedProxy.Status.Message, "Failed to read server proxy configuration:")
	require.Equal(t, failedProxy.Status.Message, completedProxy.Status.Message)
	require.True(t, process.IsProcessGoneErr(teInfo.TestProcessExecutor.CheckProcessRunning(handle)))
	stdoutFile, stdoutOK := execution.Cmd.Stdout.(*os.File)
	stderrFile, stderrOK := execution.Cmd.Stderr.(*os.File)
	require.True(t, stdoutOK)
	require.True(t, stderrOK)
	require.NoFileExists(t, stdoutFile.Name())
	require.NoFileExists(t, stderrFile.Name())
	require.Len(t, teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil), 1)
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Status.Message = "forcing another terminal-state reconciliation"
		return serverInfo.Client.Status().Update(updateCtx, current)
	}))
	completedProxy = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed &&
			current.Status.Message == failedProxy.Status.Message &&
			current.Status.ServerProxyProcessID == nil &&
			current.Status.ClientProxyContainerID == "", nil
	})
	require.Len(t, teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil), 1)
	require.NoError(t, serverInfo.Client.Delete(ctx, completedProxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, completedProxy)
	require.Len(t, teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil), 1)
}

// Verifies that deleting a tunnel proxy during server startup removes its API object,
// stops the server process, removes the client container and private credentials, and closes the output files.
func TestTunnelProxyCleansUpServerStartedDuringDeletion(t *testing.T) {
	t.Parallel()
	dcppaths.EnableTestPathProbing()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, teInfo, environmentErr := StartTestEnvironment(
		t, ctx, ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir(),
	)
	require.NoError(t, environmentErr)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "startup-deletion-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)
	configBytes, configErr := json.Marshal(dcptun.TunnelProxyConfig{ServerControlPort: 15682})
	require.NoError(t, configErr)
	readinessBytes, readinessErr := json.Marshal(dcptun.TunnelProxyReadiness{ControlReady: true})
	require.NoError(t, readinessErr)
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}}
	releaseStartup := make(chan struct{})
	finishStartup := sync.OnceFunc(func() { close(releaseStartup) })
	defer finishStartup()
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			select {
			case <-releaseStartup:
			case <-ctx.Done():
				return 1
			}
			if _, writeErr := execution.Cmd.Stdout.Write(osutil.WithNewline(configBytes)); writeErr != nil {
				t.Errorf("could not write server configuration: %v", writeErr)
				return 1
			}
			if _, writeErr := execution.Cmd.Stdout.Write(osutil.WithNewline(readinessBytes)); writeErr != nil {
				t.Errorf("could not write server readiness: %v", writeErr)
				return 1
			}
			select {
			case _, open := <-execution.Signal:
				if !open {
					t.Error("server signal channel closed unexpectedly")
				}
			case <-ctx.Done():
			}
			return 0
		},
	})
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "startup-deletion-proxy"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))
	var oldExecution *internal_testutil.ProcessExecution
	startedErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		executions := teInfo.TestProcessExecutor.FindAll(criteria.Command, "", nil)
		if len(executions) != 1 {
			return false, nil
		}
		oldExecution = executions[0]
		return true, nil
	})
	require.NoError(t, startedErr)
	startingProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateStarting, nil
	})
	physicalContainer := &apiv2.PhysicalContainer{}
	require.NoError(t, serverInfo.Client.Get(ctx, types.NamespacedName{
		Name: fmt.Sprintf("tunnel-proxy-%s", proxy.UID), Namespace: controllers.V1PhysicalResourcesNamespaceName,
	}, physicalContainer))
	require.NotEmpty(t, physicalContainer.Status.ContainerID)
	securityPath := physicalContainer.Spec.Container.CreateFiles[0].Entries[0].Source
	require.FileExists(t, securityPath)
	require.NoError(t, serverInfo.Client.Delete(ctx, startingProxy))
	finishStartup()
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, proxy)
	oldHandle := process.NewHandle(oldExecution.PID, oldExecution.StartedAt)
	cleanupErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(pollCtx context.Context) (bool, error) {
		runningErr := teInfo.TestProcessExecutor.CheckProcessRunning(oldHandle)
		if !process.IsProcessGoneErr(runningErr) {
			return false, runningErr
		}
		_, inspectErr := serverInfo.ContainerOrchestrator.InspectContainers(pollCtx, containers.InspectContainersOptions{
			Containers: []string{physicalContainer.Status.ContainerID},
		})
		if errors.Is(inspectErr, containers.ErrNotFound) {
			return true, nil
		}
		return false, inspectErr
	})
	require.NoError(t, cleanupErr)
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, physicalContainer)
	require.NoFileExists(t, securityPath)
	require.NoDirExists(t, filepath.Dir(securityPath))
	stdoutFile, stdoutOK := oldExecution.Cmd.Stdout.(*os.File)
	stderrFile, stderrOK := oldExecution.Cmd.Stderr.(*os.File)
	require.True(t, stdoutOK)
	require.True(t, stderrOK)
	_, stdoutErr := stdoutFile.Write(nil)
	_, stderrErr := stderrFile.Write(nil)
	require.ErrorIs(t, stdoutErr, os.ErrClosed)
	require.ErrorIs(t, stderrErr, os.ErrClosed)
}

// Verifies that ContainerNetworkTunnelProxy transitions to Failed state when client proxy container cannot be started.
func TestTunnelProxyClientContainerStartupFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-client-container-startup-failure"

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, _, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

// Verifies that ContainerNetworkTunnelProxy repeatedly replaces a lost server
// and its client container, including an unexpected exit with code zero.
func TestTunnelProxyServerUnexpectedExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-server-unexpected-exit"

	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironment(t, ctx, includedControllers, t.Name(), t.TempDir())
	require.NoError(t, startupErr, "Failed to start the API server")

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

	for _, exitCode := range []int32{0, 3, 0} {
		previousPID := *updatedTunnelProxy.Status.ServerProxyProcessID
		previousContainer := updatedTunnelProxy.Status.ClientProxyContainerID
		previousStdout := updatedTunnelProxy.Status.ServerProxyStdOutFile
		previousStderr := updatedTunnelProxy.Status.ServerProxyStdErrFile
		t.Logf("Simulating server proxy process exit with code %d (PID: %d)...", exitCode, serverPID)
		teInfo.TestProcessExecutor.SimulateProcessExit(t, serverPID, exitCode)
		updatedTunnelProxy = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
			return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
				tp.Status.ServerProxyProcessID != nil &&
				*tp.Status.ServerProxyProcessID != previousPID &&
				tp.Status.ClientProxyContainerID != "" &&
				tp.Status.ClientProxyContainerID != previousContainer, nil
		})
		require.Equal(t, tunnelProxy.UID, updatedTunnelProxy.UID)
		require.NotEqual(t, previousStdout, updatedTunnelProxy.Status.ServerProxyStdOutFile)
		require.NotEqual(t, previousStderr, updatedTunnelProxy.Status.ServerProxyStdErrFile)
		require.NoFileExists(t, previousStdout, "Replaced generation stdout file must be removed")
		require.NoFileExists(t, previousStderr, "Replaced generation stderr file must be removed")
		require.FileExists(t, updatedTunnelProxy.Status.ServerProxyStdOutFile)
		require.FileExists(t, updatedTunnelProxy.Status.ServerProxyStdErrFile)
		serverPID, pidErr = process.Int64_ToPidT(*updatedTunnelProxy.Status.ServerProxyProcessID)
		require.NoError(t, pidErr)
		ctrs, listErr = serverInfo.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
		require.NoError(t, listErr)
		require.Len(t, ctrs, 1)
		require.Equal(t, updatedTunnelProxy.Status.ClientProxyContainerID, ctrs[0].Id)
		require.Equal(t, containers.ContainerStatusRunning, ctrs[0].Status)
		_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), len(tunnelProxy.Spec.Tunnels), apiv1.TunnelStateNotReady)
	}
}

// Verifies that ContainerNetworkTunnelProxy recreates a client container that exits.
func TestTunnelProxyClientExited(t *testing.T) {
	t.Parallel()
	testTunnelProxyClientFailure(t, "test-tunnel-proxy-client-unexpected-exit", "Runtime container has exited", func(
		ctx context.Context,
		orchestrator *ctrl_testutil.TestContainerOrchestrator,
		containerID string,
	) error {
		return orchestrator.SimulateContainerExit(ctx, containerID, 5)
	}, nil)
}

// Verifies that ContainerNetworkTunnelProxy recreates a client container removed from the runtime.
func TestTunnelProxyClientDestroyed(t *testing.T) {
	t.Parallel()
	testTunnelProxyClientFailure(t, "test-tunnel-proxy-client-destroyed", "Runtime container was not found", func(
		ctx context.Context,
		orchestrator *ctrl_testutil.TestContainerOrchestrator,
		containerID string,
	) error {
		removedContainers, removeErr := orchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
			Containers: []string{containerID},
			Force:      true,
		})
		if removeErr != nil {
			return removeErr
		}
		if !std_slices.Equal(removedContainers, []string{containerID}) {
			return fmt.Errorf("unexpected removed containers: %v", removedContainers)
		}
		return nil
	}, nil)
}

// Verifies that ContainerNetworkTunnelProxy replaces a paused client container.
func TestTunnelProxyClientPaused(t *testing.T) {
	t.Parallel()
	testTunnelProxyClientFailure(t, "test-tunnel-proxy-client-paused", "Runtime container is paused", func(
		ctx context.Context,
		orchestrator *ctrl_testutil.TestContainerOrchestrator,
		containerID string,
	) error {
		return orchestrator.SimulateContainerStatus(ctx, containerID, containers.ContainerStatusPaused)
	}, func(ctx context.Context, orchestrator *ctrl_testutil.TestContainerOrchestrator, containerID string) error {
		return orchestrator.SimulateContainerStatus(ctx, containerID, containers.ContainerStatusRunning)
	})
}

// Verifies that ContainerNetworkTunnelProxy replaces a client whose published control/data ports disappear.
func TestTunnelProxyClientPortMappingsMissing(t *testing.T) {
	t.Parallel()
	testTunnelProxyClientFailure(t, "test-tunnel-proxy-client-port-mappings-missing", "no host port mapping exists", func(
		ctx context.Context,
		orchestrator *ctrl_testutil.TestContainerOrchestrator,
		containerID string,
	) error {
		return orchestrator.SimulateContainerPortMappings(ctx, containerID, nil)
	}, nil)
}

// Verifies that ContainerNetworkTunnelProxy recreates an unusable client and its
// server under the same logical resource, without retaining terminal tunnel states.
func testTunnelProxyClientFailure(
	t *testing.T,
	testName string,
	expectedMessage string,
	terminateContainer func(context.Context, *ctrl_testutil.TestContainerOrchestrator, string) error,
	afterFailure func(context.Context, *ctrl_testutil.TestContainerOrchestrator, string) error,
) {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	recoveryMessages := make(chan string, 16)
	includedControllers := ServiceController | NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, teInfo, startupErr := StartTestEnvironmentWithOptions(t, ctx, includedControllers, t.Name(), t.TempDir(), TestEnvironmentOptions{
		DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
			return &tunnelProxyRecoveryMessageClient{
				Client:   client,
				messages: recoveryMessages,
			}
		},
	})
	require.NoError(t, startupErr, "Failed to start the API server")

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

	testContainerOrchestrator, isTestContainerOrchestrator := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestContainerOrchestrator)
	t.Logf("Terminating client proxy runtime container (container ID: %s)...", clientContainerID)
	require.NoError(t, terminateContainer(ctx, testContainerOrchestrator, clientContainerID))

	var observedRecoveryMessage string
	select {
	case observedRecoveryMessage = <-recoveryMessages:
	case <-ctx.Done():
		t.Fatal("Tunnel proxy did not publish a recovery status message after client failure")
	}
	require.Contains(t, observedRecoveryMessage, expectedMessage)
	if afterFailure != nil {
		// Permit old-container cleanup after ContainerNetworkTunnelProxy detects the failure.
		require.NoError(t, afterFailure(ctx, testContainerOrchestrator, clientContainerID))
	}
	recovered := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			tp.Status.ServerProxyProcessID != nil &&
			*tp.Status.ServerProxyProcessID != *updatedTunnelProxy.Status.ServerProxyProcessID &&
			tp.Status.ClientProxyContainerID != "" &&
			tp.Status.ClientProxyContainerID != clientContainerID, nil
	})
	require.Equal(t, updatedTunnelProxy.UID, recovered.UID)
	ctrs, listErr = serverInfo.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
	require.NoError(t, listErr)
	require.Len(t, ctrs, 1)
	require.Equal(t, recovered.Status.ClientProxyContainerID, ctrs[0].Id)

	t.Log("Verifying server proxy process has been stopped...")
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true /* poll immediately */, func(_ context.Context) (bool, error) {
		pe, found = teInfo.TestProcessExecutor.FindByPid(serverPID)
		if !found {
			return false, fmt.Errorf("server proxy process not found")
		}
		return pe.Finished(), nil
	})
	require.NoError(t, waitErr, "Expected the previous server proxy process to have been stopped during recovery")

	_ = waitAllTunnelsInState(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), len(tunnelProxy.Spec.Tunnels), apiv1.TunnelStateNotReady)
}

// Verifies that ContainerNetworkTunnelProxy retries failed replacement containers
// without a total restart limit or resetting its pair-recovery backoff, even when
// any generation that established its control stream would be considered stable.
func TestTunnelProxyRecoveryRetainsBackoffAcrossFailedGenerations(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	policy := &observedTunnelRecoveryBackoff{BackOff: backoff.NewConstantBackOff(20 * time.Millisecond)}
	var factories atomic.Uint32
	server, environment, running := createRunningTunnelProxy(t, ctx, "recovery-backoff", TestEnvironmentOptions{
		ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
			config.PairRestartBackoffFactory = func() backoff.BackOff {
				factories.Add(1)
				return policy
			}
			config.PairRestartBackoffResetAfter = time.Nanosecond
		},
	})
	orchestrator, isTestOrchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	failureCtx, stopFailures := context.WithCancel(ctx)
	defer stopFailures()
	orchestrator.FailMatchingContainers(failureCtx, dcptun.ClientProxyContainerImageNamePrefix, 7, "temporary replacement startup failure")
	serverPID, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	environment.SimulateProcessExit(t, serverPID, 0)
	retryErr := wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
		return policy.attempts.Load() >= 4, nil
	})
	require.NoError(t, retryErr)
	stopFailures()
	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			current.Status.ClientProxyContainerID != "" && current.Status.ClientProxyContainerID != running.Status.ClientProxyContainerID, nil
	})
	require.Equal(t, running.UID, recovered.UID)
	require.Equal(t, uint32(1), factories.Load())
	require.Zero(t, policy.resets.Load())
}

// Verifies that ContainerNetworkTunnelProxy resets its pair restart backoff when a replacement pair generation fails
// after running for at least PairRestartBackoffResetAfter since its first control stream, and retains the backoff otherwise.
func TestTunnelProxyRecoveryResetsBackoffAfterStableGeneration(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		resetAfter     time.Duration
		expectedResets uint32
	}{
		{name: "stable-generation", resetAfter: time.Nanosecond, expectedResets: 1},
		{name: "short-lived-generation", resetAfter: time.Hour, expectedResets: 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			policy := &observedTunnelRecoveryBackoff{BackOff: backoff.NewConstantBackOff(20 * time.Millisecond)}
			var factories atomic.Uint32
			server, environment, running := createRunningTunnelProxy(t, ctx, "recovery-backoff-reset-"+tc.name, TestEnvironmentOptions{
				ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
					config.PairRestartBackoffFactory = func() backoff.BackOff {
						factories.Add(1)
						return policy
					}
					config.PairRestartBackoffResetAfter = tc.resetAfter
				},
			})

			// The first recovery creates the backoff; the second one follows a replacement generation that became ready.
			current := running
			for range 2 {
				previous := current
				serverPID, pidErr := process.Int64_ToPidT(*previous.Status.ServerProxyProcessID)
				require.NoError(t, pidErr)
				environment.SimulateProcessExit(t, serverPID, 0)
				current = waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(proxy *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
					return proxy.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
						proxy.Status.ServerProxyProcessID != nil &&
						*proxy.Status.ServerProxyProcessID != *previous.Status.ServerProxyProcessID &&
						proxy.Status.ClientProxyContainerID != "" &&
						proxy.Status.ClientProxyContainerID != previous.Status.ClientProxyContainerID, nil
				})
			}

			require.Equal(t, running.UID, current.UID)
			require.Equal(t, uint32(1), factories.Load())
			require.Equal(t, uint32(2), policy.attempts.Load())
			require.Equal(t, tc.expectedResets, policy.resets.Load())
		})
	}
}

// Verifies that ContainerNetworkTunnelProxy publishes distinct recovery phases
// and defers readiness withdrawal and physical cleanup to subsequent reconciliations.
func TestTunnelProxyRecoveryUsesSeparateStateInitializers(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	resumeDetection, resumePreparation := make(chan struct{}), make(chan struct{})
	releaseDetection := sync.OnceFunc(func() { close(resumeDetection) })
	releasePreparation := sync.OnceFunc(func() { close(resumePreparation) })
	defer releaseDetection()
	defer releasePreparation()
	gate := &tunnelProxyStatusGate{
		checkpoints: make(chan *apiv1.ContainerNetworkTunnelProxy, 2),
		resume:      [2]<-chan struct{}{resumeDetection, resumePreparation},
		states: [2]apiv1.ContainerNetworkTunnelProxyState{
			apiv1.ContainerNetworkTunnelProxyStateRecoveryPending,
			apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup,
		},
	}
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir(), TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				gate.Client = client
				return gate
			},
		})
	require.NoError(t, startupErr)
	const name = "recovery-state-initializers"
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: name + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	simulateServerProxy(t, 16784, environment.TestProcessExecutor)
	tunnel := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 1, name, 28272)[0]
	resource := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{{
				Name: tunnel.tunnelName, ServerServiceName: tunnel.serverServiceName, ClientServiceName: tunnel.clientServiceName,
			}},
		},
	}
	require.NoError(t, server.Client.Create(ctx, resource))
	running := waitAllTunnelsInState(t, ctx, server.Client, resource.NamespacedName(), 1, apiv1.TunnelStateReady)
	orchestrator, isTestOrchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	pid, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	handle := process.NewHandle(pid, running.Status.ServerProxyStartupTimestamp.Time)
	physicalName := types.NamespacedName{
		Namespace: controllers.V1PhysicalResourcesNamespaceName,
		Name:      "tunnel-proxy-" + string(resource.UID),
	}
	gate.stage.Store(1)
	require.NoError(t, orchestrator.SimulateContainerPortMappings(ctx, running.Status.ClientProxyContainerID, nil))

	for _, step := range []struct {
		proxyState  apiv1.ContainerNetworkTunnelProxyState
		tunnelState apiv1.TunnelState
		resume      func()
	}{
		{apiv1.ContainerNetworkTunnelProxyStateRecoveryPending, apiv1.TunnelStateReady, releaseDetection},
		{apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup, apiv1.TunnelStateNotReady, releasePreparation},
	} {
		var checkpoint *apiv1.ContainerNetworkTunnelProxy
		select {
		case checkpoint = <-gate.checkpoints:
		case <-ctx.Done():
			t.Fatal("Recovery did not reach its next reconciliation boundary")
		}
		require.Equal(t, step.proxyState, checkpoint.Status.State)
		published := &apiv1.ContainerNetworkTunnelProxy{}
		require.NoError(t, server.Client.Get(ctx, resource.NamespacedName(), published))
		require.Equal(t, step.proxyState, published.Status.State)
		require.Len(t, checkpoint.Status.TunnelStatuses, 1)
		require.Equal(t, step.tunnelState, checkpoint.Status.TunnelStatuses[0].State)
		physical := &apiv2.PhysicalContainer{}
		require.NoError(t, server.Client.Get(ctx, physicalName, physical))
		require.Nil(t, physical.DeletionTimestamp, "Cleanup must not start before the cleanup initializer runs")
		require.NoError(t, environment.TestProcessExecutor.CheckProcessRunning(handle))
		step.resume()
	}
	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			current.Status.ClientProxyContainerID != "" &&
			current.Status.ClientProxyContainerID != running.Status.ClientProxyContainerID &&
			len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv1.TunnelStateReady, nil
	})
	require.Equal(t, running.Status.TunnelStatuses[0].ClientProxyPort, recovered.Status.TunnelStatuses[0].ClientProxyPort)
}

// Verifies that ContainerNetworkTunnelProxy recovery keeps a tunnel that failed on the old pair Failed with its
// reason until the replacement pair is created, refreshes the timestamps of tunnels it marks NotReady, carries the
// statuses of desired tunnels to the replacement pair with their published ports, and prepares every tunnel again.
func TestTunnelProxyRecoveryCarriesTunnelStatusesToReplacementPair(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	resumeWait, resumeReplacement := make(chan struct{}), make(chan struct{})
	releaseWait := sync.OnceFunc(func() { close(resumeWait) })
	releaseReplacement := sync.OnceFunc(func() { close(resumeReplacement) })
	defer releaseWait()
	defer releaseReplacement()
	gate := &tunnelProxyStatusGate{
		checkpoints: make(chan *apiv1.ContainerNetworkTunnelProxy, 2),
		resume:      [2]<-chan struct{}{resumeWait, resumeReplacement},
		states: [2]apiv1.ContainerNetworkTunnelProxyState{
			apiv1.ContainerNetworkTunnelProxyStateRecoveryWait,
			apiv1.ContainerNetworkTunnelProxyStateBuildingImage,
		},
	}
	const testName = "recovery-carries-tunnel-statuses"
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir(), TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				gate.Client = client
				return gate
			},
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				config.PairRestartBackoffFactory = func() backoff.BackOff {
					return backoff.NewConstantBackOff(100 * time.Millisecond)
				}
			},
		})
	require.NoError(t, startupErr)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	simulateServerProxy(t, 16805, environment.TestProcessExecutor)
	tunnels := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 2, testName, 31110)
	readyTunnel, failingTunnel := tunnels[0], tunnels[1]
	require.True(t, environment.TestTunnelControlClient.DisableTunnel(failingTunnel.fingerprint))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{Name: readyTunnel.tunnelName, ServerServiceName: readyTunnel.serverServiceName, ClientServiceName: readyTunnel.clientServiceName},
				{Name: failingTunnel.tunnelName, ServerServiceName: failingTunnel.serverServiceName, ClientServiceName: failingTunnel.clientServiceName},
			},
		},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		statuses := maps.SliceToMap(current.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			statuses[readyTunnel.tunnelName].State == apiv1.TunnelStateReady &&
			statuses[failingTunnel.tunnelName].State == apiv1.TunnelStateFailed, nil
	})
	runningStatuses := maps.SliceToMap(running.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	readyBefore, failedBefore := runningStatuses[readyTunnel.tunnelName], runningStatuses[failingTunnel.tunnelName]
	require.NotEmpty(t, failedBefore.ErrorMessage)

	// The current pair does not retry the failed tunnel; only the replacement pair can prepare it.
	environment.TestTunnelControlClient.EnableTunnel(&dcptunproto.TunnelSpec{
		ServerAddress:   stdproto.String(failingTunnel.fingerprint.ServerAddress),
		ServerPort:      stdproto.Int32(failingTunnel.fingerprint.ServerPort),
		ClientProxyPort: stdproto.Int32(failingTunnel.fingerprint.ClientProxyPort),
	})
	gate.stage.Store(1)
	serverPID, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	environment.TestProcessExecutor.SimulateProcessExit(t, serverPID, 0)

	awaitCheckpoint := func(description string) *apiv1.ContainerNetworkTunnelProxy {
		select {
		case checkpoint := <-gate.checkpoints:
			return checkpoint
		case <-ctx.Done():
			t.Fatalf("Recovery did not reach %s", description)
			return nil
		}
	}

	waiting := awaitCheckpoint("RecoveryWait")
	require.NotEmpty(t, waiting.Status.Message)
	waitingStatuses := maps.SliceToMap(waiting.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Len(t, waitingStatuses, 2)
	readyWaiting, failedWaiting := waitingStatuses[readyTunnel.tunnelName], waitingStatuses[failingTunnel.tunnelName]
	require.Equal(t, apiv1.TunnelStateNotReady, readyWaiting.State)
	require.Equal(t, waiting.Status.Message, readyWaiting.ErrorMessage)
	require.True(t, readyWaiting.Timestamp.After(readyBefore.Timestamp.Time), "Marking a tunnel NotReady must refresh its timestamp")
	require.Equal(t, readyBefore.ClientProxyPort, readyWaiting.ClientProxyPort)
	require.Equal(t, failedBefore, failedWaiting, "A tunnel that failed on the old pair must keep its status until the replacement pair is created")
	releaseWait()

	replacement := awaitCheckpoint("replacement pair startup")
	replacementStatuses := maps.SliceToMap(replacement.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Len(t, replacementStatuses, 2, "Replacement startup must keep the statuses of desired tunnels")
	readyCarried, failedCarried := replacementStatuses[readyTunnel.tunnelName], replacementStatuses[failingTunnel.tunnelName]
	for _, carried := range []apiv1.TunnelStatus{readyCarried, failedCarried} {
		require.Equal(t, apiv1.TunnelStateNotReady, carried.State)
		require.Zero(t, carried.TunnelID, "Tunnel IDs belong to the replaced pair")
		require.Empty(t, carried.ClientProxyAddresses, "Listener addresses belong to the replaced pair")
	}
	require.Equal(t, readyBefore.ClientProxyPort, readyCarried.ClientProxyPort, "The published port must remain visible during replacement startup")
	require.Equal(t, waiting.Status.Message, readyCarried.ErrorMessage)
	require.True(t, readyCarried.Timestamp.After(readyWaiting.Timestamp.Time))
	require.Zero(t, failedCarried.ClientProxyPort)
	require.Equal(t, failedBefore.ErrorMessage, failedCarried.ErrorMessage, "A previously failed tunnel keeps its failure reason until it is retried")
	require.True(t, failedCarried.Timestamp.After(failedBefore.Timestamp.Time))
	releaseReplacement()

	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			current.Status.ClientProxyContainerID != running.Status.ClientProxyContainerID &&
			len(current.Status.TunnelStatuses) == 2 &&
			slices.All(current.Status.TunnelStatuses, func(ts apiv1.TunnelStatus) bool { return ts.State == apiv1.TunnelStateReady }), nil
	})
	recoveredStatuses := maps.SliceToMap(recovered.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Equal(t, readyBefore.ClientProxyPort, recoveredStatuses[readyTunnel.tunnelName].ClientProxyPort)
	require.Empty(t, recoveredStatuses[failingTunnel.tunnelName].ErrorMessage)
	require.NotZero(t, recoveredStatuses[failingTunnel.tunnelName].TunnelID)
}

type tunnelProxyStatusGate struct {
	ctrl_client.Client
	stage        atomic.Uint32
	checkpoints  chan *apiv1.ContainerNetworkTunnelProxy
	resume       [2]<-chan struct{}
	states       [2]apiv1.ContainerNetworkTunnelProxyState
	readObserver *tunnelProxyDeletedStateReadObserver
}

type tunnelProxyDeletedStateReadObserver struct {
	ctrl_client.Reader
	armed               atomic.Bool
	deletedPhysicalName string
	notFound            chan struct{}
	physicalGet         chan struct{}
	notFoundOnce        sync.Once
	physicalOnce        sync.Once
}

func (observer *tunnelProxyDeletedStateReadObserver) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	getErr := observer.Reader.Get(ctx, key, object, options...)
	observer.observeGet(key, object, getErr, true)
	return getErr
}

func (observer *tunnelProxyDeletedStateReadObserver) observeGet(
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	getErr error,
	observePhysicalContainer bool,
) {
	if !observer.armed.Load() {
		return
	}

	if _, isProxy := object.(*apiv1.ContainerNetworkTunnelProxy); isProxy && apimachinery_errors.IsNotFound(getErr) {
		observer.notFoundOnce.Do(func() { close(observer.notFound) })
		return
	}
	if _, isPhysicalContainer := object.(*apiv2.PhysicalContainer); observePhysicalContainer && isPhysicalContainer &&
		(observer.deletedPhysicalName == "" || key.Name == observer.deletedPhysicalName) {
		select {
		case <-observer.notFound:
			observer.physicalOnce.Do(func() { close(observer.physicalGet) })
		default:
		}
	}
}

func (gate *tunnelProxyStatusGate) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	getErr := gate.Client.Get(ctx, key, object, options...)
	if gate.readObserver != nil {
		gate.readObserver.observeGet(key, object, getErr, false)
	}
	return getErr
}

func (gate *tunnelProxyStatusGate) Status() ctrl_client.SubResourceWriter {
	return &tunnelProxyStatusWriter{SubResourceWriter: gate.Client.Status(), gate: gate}
}

type tunnelProxyStatusWriter struct {
	ctrl_client.SubResourceWriter
	gate *tunnelProxyStatusGate
}

func (writer *tunnelProxyStatusWriter) Patch(ctx context.Context, object ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	patchErr := writer.SubResourceWriter.Patch(ctx, object, patch, options...)
	if patchErr != nil {
		return patchErr
	}
	proxy, isProxy := object.(*apiv1.ContainerNetworkTunnelProxy)
	if isProxy {
		stage := writer.gate.stage.Load()
		if stage >= 1 && stage <= 2 && proxy.Status.State == writer.gate.states[stage-1] && writer.gate.stage.CompareAndSwap(stage, stage+1) {
			select {
			case writer.gate.checkpoints <- proxy.DeepCopy():
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-writer.gate.resume[stage-1]:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

// Verifies that ContainerNetworkTunnelProxy restores externally changed API
// lifecycle states without restarting or deleting either member of the pair.
func TestTunnelProxyAPIStateIsProjection(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, running := createRunningTunnelProxy(t, ctx, "lifecycle-status-projection", TestEnvironmentOptions{})
	for _, state := range []apiv1.ContainerNetworkTunnelProxyState{
		apiv1.ContainerNetworkTunnelProxyStateEmpty,
		apiv1.ContainerNetworkTunnelProxyStateStarted,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryPending,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryWait,
		apiv1.ContainerNetworkTunnelProxyStateFailed,
		apiv1.ContainerNetworkTunnelProxyStateDeletePending,
		apiv1.ContainerNetworkTunnelProxyStateDeleting,
		apiv1.ContainerNetworkTunnelProxyStateDeleted,
	} {
		require.NoError(t, retryOnConflictEx(ctx, server.Client, running.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
			current.Status.State = state
			return server.Client.Status().Update(updateCtx, current)
		}))
		projected := waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
			return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
		})
		require.Equal(t, running.Status.ServerProxyProcessID, projected.Status.ServerProxyProcessID)
		require.Equal(t, running.Status.ServerProxyStartupTimestamp, projected.Status.ServerProxyStartupTimestamp)
		require.Equal(t, running.Status.ClientProxyContainerID, projected.Status.ClientProxyContainerID)
	}
}

// Verifies that ContainerNetworkTunnelProxy publishes Deleting and Deleted through
// the API before releasing its finalizer, without repeating completed cleanup.
func TestTunnelProxyPublishesDeletionStates(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resumeDeleting, resumeDeleted := make(chan struct{}), make(chan struct{})
	releaseDeleting := sync.OnceFunc(func() { close(resumeDeleting) })
	releaseDeleted := sync.OnceFunc(func() { close(resumeDeleted) })
	defer releaseDeleting()
	defer releaseDeleted()
	gate := &tunnelProxyStatusGate{
		checkpoints: make(chan *apiv1.ContainerNetworkTunnelProxy, 2),
		resume:      [2]<-chan struct{}{resumeDeleting, resumeDeleted},
		states: [2]apiv1.ContainerNetworkTunnelProxyState{
			apiv1.ContainerNetworkTunnelProxyStateDeleting,
			apiv1.ContainerNetworkTunnelProxyStateDeleted,
		},
	}
	readObserver := &tunnelProxyDeletedStateReadObserver{
		notFound:    make(chan struct{}),
		physicalGet: make(chan struct{}),
	}
	gate.readObserver = readObserver
	server, environment, running := createRunningTunnelProxy(t, ctx, "published-deletion-states", TestEnvironmentOptions{
		DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			gate.Client = client
			return gate
		},
		DecorateTunnelProxyAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
			readObserver.Reader = reader
			return readObserver
		},
	})
	const hold = "test.dcp.microsoft.com/hold-tunnel-deletion"
	require.NoError(t, retryOnConflictEx(ctx, server.Client, running.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Finalizers = append(current.Finalizers, hold)
		return server.Client.Update(updateCtx, current)
	}))
	gate.stage.Store(1)
	require.NoError(t, server.Client.Delete(ctx, running))
	for _, step := range []struct {
		state  apiv1.ContainerNetworkTunnelProxyState
		resume func()
	}{
		{apiv1.ContainerNetworkTunnelProxyStateDeleting, releaseDeleting},
		{apiv1.ContainerNetworkTunnelProxyStateDeleted, releaseDeleted},
	} {
		var checkpoint *apiv1.ContainerNetworkTunnelProxy
		select {
		case checkpoint = <-gate.checkpoints:
		case <-ctx.Done():
			t.Fatal("Deletion did not publish its next lifecycle state")
		}
		require.Equal(t, step.state, checkpoint.Status.State)
		published := &apiv1.ContainerNetworkTunnelProxy{}
		require.NoError(t, server.Client.Get(ctx, running.NamespacedName(), published))
		require.Equal(t, step.state, published.Status.State)
		require.False(t, published.DeletionTimestamp.IsZero())
		require.Contains(t, published.Finalizers, hold)
		if step.state == apiv1.ContainerNetworkTunnelProxyStateDeleted {
			require.Nil(t, published.Status.ServerProxyProcessID)
			require.Empty(t, published.Status.ClientProxyContainerID)
		}
		step.resume()
	}
	controllerFinalizer := apiv1.GroupVersion.Group + "/tunnel-proxy-reconciler"
	completed := waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateDeleted &&
			!slices.Contains(current.Finalizers, controllerFinalizer), nil
	})
	require.Contains(t, completed.Finalizers, hold)
	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	require.Len(t, environment.TestProcessExecutor.FindAll([]string{dcpPath, "tunnel-server"}, "", nil), 1)
	remainingContainers, listErr := server.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
	require.NoError(t, listErr)
	require.Empty(t, remainingContainers)
	readObserver.deletedPhysicalName = fmt.Sprintf("tunnel-proxy-%s", running.UID)
	readObserver.armed.Store(true)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, running.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Finalizers = std_slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == hold })
		return server.Client.Update(updateCtx, current)
	}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, running)
	select {
	case <-readObserver.notFound:
	case <-ctx.Done():
		t.Fatal("Tunnel proxy controller did not reconcile the removed object")
	}

	// Recreate the same NamespacedName and wait for it to reach Running: controller-runtime
	// serializes Reconcile per key and startProxyPair runs on workQueue, proving the prior
	// NotFound reconciliation finished without scheduling abandoned physical cleanup.
	recreated := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      running.Name,
			Namespace: running.Namespace,
		},
		Spec: running.Spec,
	}
	require.NoError(t, server.Client.Create(ctx, recreated))
	_ = waitObjectAssumesStateEx(t, ctx, server.Client, recreated.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
	select {
	case <-readObserver.physicalGet:
		t.Fatal("Completed tunnel proxy deletion must not run abandoned-startup physical cleanup")
	default:
	}
}

type observedTunnelRecoveryBackoff struct {
	backoff.BackOff
	attempts atomic.Uint32
	resets   atomic.Uint32
}

func (policy *observedTunnelRecoveryBackoff) NextBackOff() time.Duration {
	policy.attempts.Add(1)
	return policy.BackOff.NextBackOff()
}

func (policy *observedTunnelRecoveryBackoff) Reset() {
	policy.resets.Add(1)
	policy.BackOff.Reset()
}

// Verifies that deleting ContainerNetworkTunnelProxy during recovery cancels a
// pending replacement delay and leaves no client container or server process.
func TestTunnelProxyDeletionDuringRecovery(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	policy := &observedTunnelRecoveryBackoff{BackOff: backoff.NewConstantBackOff(time.Hour)}
	server, environment, running := createRunningTunnelProxy(t, ctx, "delete-during-recovery", TestEnvironmentOptions{
		ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
			config.PairRestartBackoffFactory = func() backoff.BackOff { return policy }
		},
	})
	serverPID, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	execution, found := environment.FindByPid(serverPID)
	require.True(t, found)
	environment.SimulateProcessExit(t, serverPID, 0)
	waiting := waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRecoveryWait, nil
	})
	require.Equal(t, uint32(1), policy.attempts.Load())
	require.Nil(t, waiting.Status.ServerProxyProcessID)
	require.Empty(t, waiting.Status.ClientProxyContainerID)
	require.NoError(t, server.Client.Delete(ctx, running))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, running)
	require.Len(t, environment.TestProcessExecutor.FindAll([]string{execution.Cmd.Path, "tunnel-server"}, "", nil), 1)
	remainingContainers, listErr := server.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
	require.NoError(t, listErr)
	require.Empty(t, remainingContainers)
}

// Verifies that removing a tunnel during RecoveryWait drops its durable port
// reservation so a later tunnel can claim that exact port.
func TestTunnelProxyRecoveryPrunesRemovedPortReservations(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	const testName = "recovery-prunes-removed-port"
	server, environment, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				config.PairRestartBackoffFactory = func() backoff.BackOff {
					return backoff.NewConstantBackOff(2 * time.Second)
				}
			},
		},
	)
	require.NoError(t, startupErr)
	simulateServerProxy(t, 16787, environment.TestProcessExecutor)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	tunnels := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 2, testName, 31020)
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{Name: tunnels[0].tunnelName, ServerServiceName: tunnels[0].serverServiceName, ClientServiceName: tunnels[0].clientServiceName},
				{Name: tunnels[1].tunnelName, ServerServiceName: tunnels[1].serverServiceName, ClientServiceName: tunnels[1].clientServiceName},
			},
		},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 2, apiv1.TunnelStateReady)
	runningStatuses := maps.SliceToMap(running.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	removedPort := runningStatuses[tunnels[1].tunnelName].ClientProxyPort

	serverPID, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	environment.SimulateProcessExit(t, serverPID, 0)
	waiting := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRecoveryWait, nil
	})
	waiting.Spec.Tunnels = waiting.Spec.Tunnels[:1]
	require.NoError(t, server.Client.Update(ctx, waiting))
	require.True(t, environment.TestTunnelControlClient.DisableTunnel(tunnels[1].fingerprint))

	recovered := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	addedTunnel := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 1, testName+"-replacement", removedPort)[0]
	recovered.Spec.Tunnels = append(recovered.Spec.Tunnels, apiv1.TunnelConfiguration{
		Name:              addedTunnel.tunnelName,
		ServerServiceName: addedTunnel.serverServiceName,
		ClientServiceName: addedTunnel.clientServiceName,
	})
	require.NoError(t, server.Client.Update(ctx, recovered))

	updated := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 2, apiv1.TunnelStateReady)
	updatedStatuses := maps.SliceToMap(updated.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Equal(t, removedPort, updatedStatuses[addedTunnel.tunnelName].ClientProxyPort)
}

// Verifies that invalid replacement-server configuration remains terminal after
// an earlier successful generation, rather than entering a recovery relaunch loop.
func TestTunnelProxyRecoveryConfigurationFailureIsTerminal(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, running := createRunningTunnelProxy(t, ctx, "recovery-invalid-config", TestEnvironmentOptions{})
	serverPID, pidErr := process.Int64_ToPidT(*running.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	execution, found := environment.FindByPid(serverPID)
	require.True(t, found)
	criteria := internal_testutil.ProcessSearchCriteria{Command: []string{execution.Cmd.Path, "tunnel-server"}}
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: criteria,
		RunCommand: func(replacement *internal_testutil.ProcessExecution) int32 {
			if _, writeErr := io.WriteString(replacement.Cmd.Stdout, "invalid configuration\n"); writeErr != nil {
				t.Errorf("could not write replacement configuration: %v", writeErr)
				return 1
			}
			select {
			case _, open := <-replacement.Signal:
				if !open {
					t.Error("replacement signal channel closed unexpectedly")
				}
			case <-ctx.Done():
			}
			return 0
		},
	})
	environment.SimulateProcessExit(t, serverPID, 0)
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed &&
			current.Status.ServerProxyProcessID == nil && current.Status.ClientProxyContainerID == "", nil
	})
	require.Contains(t, failed.Status.Message, "Failed to read server proxy configuration:")
	require.Len(t, environment.TestProcessExecutor.FindAll(criteria.Command, "", nil), 2)
	require.NoError(t, server.Client.Delete(ctx, failed))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, failed)
	require.Len(t, environment.TestProcessExecutor.FindAll(criteria.Command, "", nil), 2)
}

// Verifies that a stale cached NotFound followed by an AlreadyExists create
// cannot clear the recorded physical UID or delete a replacement resource.
func TestTunnelProxyCreateConflictPreservesPhysicalIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	releaseConflict := make(chan struct{})
	releaseConflictOnce := sync.OnceFunc(func() { close(releaseConflict) })
	defer releaseConflictOnce()
	var staleClient *staleTunnelPhysicalContainerClient
	server, environment, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				staleClient = &staleTunnelPhysicalContainerClient{
					Client:          client,
					created:         make(chan *apiv2.PhysicalContainer, 1),
					createConflict:  make(chan struct{}),
					releaseConflict: releaseConflict,
				}
				return staleClient
			},
		},
	)
	require.NoError(t, startupErr)
	require.NotNil(t, staleClient)
	simulateServerProxy(t, 16785, environment.TestProcessExecutor)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "create-conflict-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "create-conflict-proxy"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))

	var original *apiv2.PhysicalContainer
	select {
	case original = <-staleClient.created:
	case <-ctx.Done():
		t.Fatal("Tunnel proxy did not create its initial PhysicalContainer")
	}
	select {
	case <-staleClient.createConflict:
	case <-ctx.Done():
		t.Fatalf(
			"Tunnel proxy did not encounter the injected AlreadyExists conflict (state=%d, gets=%d, creates=%d)",
			staleClient.state.Load(),
			staleClient.gets.Load(),
			staleClient.creates.Load(),
		)
	}

	require.NoError(t, server.Client.Delete(ctx, original, ctrl_client.Preconditions{UID: &original.UID}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, original)
	replacement := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{
			Name:        original.Name,
			Namespace:   original.Namespace,
			Annotations: std_maps.Clone(original.Annotations),
		},
		Spec: *original.Spec.DeepCopy(),
	}
	require.NoError(t, server.Client.Create(ctx, replacement))
	require.NotEqual(t, original.UID, replacement.UID)
	releaseConflictOnce()

	failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})
	require.Contains(t, failed.Status.Message, "was replaced")
	require.NoError(t, server.Client.Delete(ctx, failed))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, failed)

	retained := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, replacement.NamespacedName(), retained))
	require.Equal(t, replacement.UID, retained.UID)
	require.Nil(t, retained.DeletionTimestamp)
	require.NoError(t, server.Client.Delete(ctx, retained))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, retained)
}

// Verifies that an initial PhysicalContainer create conflict with foreign
// ownership is rejected and cleanup never deletes the conflicting resource.
func TestTunnelProxyCreateConflictPreservesForeignPhysicalContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	var conflictClient *foreignTunnelPhysicalContainerConflictClient
	server, environment, startupErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				conflictClient = &foreignTunnelPhysicalContainerConflictClient{
					Client:  client,
					created: make(chan *apiv2.PhysicalContainer, 1),
				}
				return conflictClient
			},
		},
	)
	require.NoError(t, startupErr)
	require.NotNil(t, conflictClient)
	simulateServerProxy(t, 16786, environment.TestProcessExecutor)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "foreign-create-conflict-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-create-conflict-proxy"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))

	var foreign *apiv2.PhysicalContainer
	select {
	case foreign = <-conflictClient.created:
	case <-ctx.Done():
		t.Fatal("Tunnel proxy did not encounter the injected foreign PhysicalContainer conflict")
	}
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})
	require.Contains(t, failed.Status.Message, "owned by a different tunnel proxy")

	retained := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, foreign.NamespacedName(), retained))
	require.Equal(t, foreign.UID, retained.UID)
	require.NoError(t, server.Client.Delete(ctx, failed))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, failed)
	require.NoError(t, server.Client.Get(ctx, foreign.NamespacedName(), retained))
	require.Equal(t, foreign.UID, retained.UID)

	require.NoError(t, server.Client.Delete(ctx, retained))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, retained)
}

// Verifies that delayed exit callbacks and cleanup from an old proxy generation
// cannot fail its replacement or delete a different physical resource with a reused name.
func TestTunnelProxyRecoveryFencesOldExitAndPhysicalIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	releaseDelete := make(chan struct{})
	unblockDelete := sync.OnceFunc(func() { close(releaseDelete) })
	defer unblockDelete()
	delayedExit := make(chan func(), 1)
	var deletion *delayedTunnelContainerDeletion
	server, environment, running := createRunningTunnelProxy(t, ctx, "recovery-stale-identity", TestEnvironmentOptions{
		ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
			config.ProcessExecutor = &delayedTunnelExitExecutor{Executor: config.ProcessExecutor, delayed: delayedExit}
		},
		DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			deletion = &delayedTunnelContainerDeletion{
				Client: client, deleted: make(chan *apiv2.PhysicalContainer, 1), release: releaseDelete,
			}
			return deletion
		},
	})
	deletion.armed.Store(true)
	orchestrator, isTestOrchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.True(t, isTestOrchestrator)
	require.NoError(t, orchestrator.SimulateContainerExit(ctx, running.Status.ClientProxyContainerID, 5))
	var oldPhysical *apiv2.PhysicalContainer
	select {
	case oldPhysical = <-deletion.deleted:
	case <-ctx.Done():
		t.Fatal("Recovery did not submit the old physical-container deletion")
	}
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, oldPhysical)
	reused := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: oldPhysical.Name, Namespace: oldPhysical.Namespace},
		Spec:       *oldPhysical.Spec.DeepCopy(),
	}
	require.NoError(t, server.Client.Create(ctx, reused))
	require.NotEqual(t, oldPhysical.UID, reused.UID)
	unblockDelete()
	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, running.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
			current.Status.ServerProxyProcessID != nil &&
			*current.Status.ServerProxyProcessID != *running.Status.ServerProxyProcessID &&
			current.Status.ClientProxyContainerID != "" && current.Status.ClientProxyContainerID != running.Status.ClientProxyContainerID, nil
	})
	select {
	case completeOldExit := <-delayedExit:
		completeOldExit()
	case <-ctx.Done():
		t.Fatal("The old server exit callback was not captured")
	}
	require.NoError(t, retryOnConflictEx(ctx, server.Client, running.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = append(current.Spec.Tunnels, apiv1.TunnelConfiguration{
			Name: "after-stale-exit", ServerServiceName: "missing-server-2", ClientServiceName: "missing-client-2",
		})
		return server.Client.Update(updateCtx, current)
	}))
	settled := waitAllTunnelsInState(t, ctx, server.Client, running.NamespacedName(), 2, apiv1.TunnelStateNotReady)
	require.Equal(t, recovered.Status.ServerProxyProcessID, settled.Status.ServerProxyProcessID)
	require.Equal(t, recovered.Status.ClientProxyContainerID, settled.Status.ClientProxyContainerID)
	stillReused := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, reused.NamespacedName(), stillReused))
	require.Equal(t, reused.UID, stillReused.UID)
	require.Nil(t, stillReused.DeletionTimestamp)
	require.NoError(t, server.Client.Delete(ctx, settled))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, settled)
	require.NoError(t, server.Client.Get(ctx, reused.NamespacedName(), stillReused))
	require.NoError(t, server.Client.Delete(ctx, reused))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, reused)
	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	require.Len(t, environment.TestProcessExecutor.FindAll([]string{dcpPath, "tunnel-server"}, "", nil), 2)
}

type tunnelEndpointFaultClient struct {
	ctrl_client.Client
	clientServiceName      string
	failNextEndpointDelete atomic.Bool
	endpointDeleteAttempts atomic.Uint32
}

func (client *tunnelEndpointFaultClient) List(
	ctx context.Context,
	list ctrl_client.ObjectList,
	options ...ctrl_client.ListOption,
) error {
	listErr := client.Client.List(ctx, list, options...)
	if listErr != nil {
		return listErr
	}
	endpoints, isEndpointList := list.(*apiv1.EndpointList)
	if !isEndpointList {
		return nil
	}
	filtered := endpoints.Items[:0]
	for _, endpoint := range endpoints.Items {
		if endpoint.Spec.ServiceName != client.clientServiceName {
			filtered = append(filtered, endpoint)
		}
	}
	endpoints.Items = filtered
	return nil
}

func (client *tunnelEndpointFaultClient) Delete(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.DeleteOption,
) error {
	if _, isEndpoint := object.(*apiv1.Endpoint); isEndpoint {
		client.endpointDeleteAttempts.Add(1)
		if client.failNextEndpointDelete.CompareAndSwap(true, false) {
			return errors.New("injected tunnel Endpoint deletion failure")
		}
	}
	return client.Client.Delete(ctx, object, options...)
}

type lostTunnelPreparationResponse struct {
	dcptunproto.TunnelControlClient
	loseNext      atomic.Bool
	awaitingRetry atomic.Bool
	firstApplied  chan *dcptunproto.TunnelSpec
	releaseFirst  chan struct{}
	retryRequest  chan *dcptunproto.TunnelReq
}

func (client *lostTunnelPreparationResponse) PrepareTunnel(
	ctx context.Context,
	request *dcptunproto.TunnelReq,
	options ...grpc.CallOption,
) (*dcptunproto.TunnelSpec, error) {
	tunnelSpec, prepareErr := client.TunnelControlClient.PrepareTunnel(ctx, request, options...)
	if prepareErr != nil {
		return nil, prepareErr
	}
	if client.loseNext.CompareAndSwap(true, false) {
		client.awaitingRetry.Store(true)
		client.firstApplied <- stdproto.CloneOf(tunnelSpec)
		select {
		case <-client.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, status.Error(codes.Unavailable, "prepare response was lost")
	}
	if client.awaitingRetry.CompareAndSwap(true, false) {
		client.retryRequest <- stdproto.CloneOf(request)
	}
	return tunnelSpec, nil
}

type staleTunnelPhysicalContainerClient struct {
	ctrl_client.Client
	state           atomic.Uint32
	gets            atomic.Uint32
	creates         atomic.Uint32
	created         chan *apiv2.PhysicalContainer
	createConflict  chan struct{}
	releaseConflict <-chan struct{}
}

func (client *staleTunnelPhysicalContainerClient) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	if _, isPhysicalContainer := object.(*apiv2.PhysicalContainer); isPhysicalContainer {
		client.gets.Add(1)
	}
	if _, isPhysicalContainer := object.(*apiv2.PhysicalContainer); isPhysicalContainer && client.state.Load() == 1 {
		return apimachinery_errors.NewNotFound(
			apiv2.GroupVersion.WithResource("physicalcontainers").GroupResource(),
			key.Name,
		)
	}
	return client.Client.Get(ctx, key, object, options...)
}

func (client *staleTunnelPhysicalContainerClient) Create(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.CreateOption,
) error {
	createErr := client.Client.Create(ctx, object, options...)
	physicalContainer, isPhysicalContainer := object.(*apiv2.PhysicalContainer)
	if !isPhysicalContainer {
		return createErr
	}
	client.creates.Add(1)
	if createErr == nil && client.state.CompareAndSwap(0, 1) {
		select {
		case client.created <- physicalContainer.DeepCopy():
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	if apimachinery_errors.IsAlreadyExists(createErr) && client.state.CompareAndSwap(1, 2) {
		close(client.createConflict)
		select {
		case <-client.releaseConflict:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return createErr
}

type foreignTunnelPhysicalContainerConflictClient struct {
	ctrl_client.Client
	injected atomic.Bool
	created  chan *apiv2.PhysicalContainer
}

func (client *foreignTunnelPhysicalContainerConflictClient) Create(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.CreateOption,
) error {
	physicalContainer, isPhysicalContainer := object.(*apiv2.PhysicalContainer)
	if !isPhysicalContainer || !client.injected.CompareAndSwap(false, true) {
		return client.Client.Create(ctx, object, options...)
	}

	foreign := physicalContainer.DeepCopy()
	foreign.Annotations = map[string]string{
		"test.dcp.microsoft.com/foreign-owner": "true",
	}
	createErr := client.Client.Create(ctx, foreign, options...)
	if createErr != nil {
		return createErr
	}
	select {
	case client.created <- foreign.DeepCopy():
	case <-ctx.Done():
		return ctx.Err()
	}
	return apimachinery_errors.NewAlreadyExists(
		apiv2.GroupVersion.WithResource("physicalcontainers").GroupResource(),
		physicalContainer.Name,
	)
}

type delayedTunnelContainerDeletion struct {
	ctrl_client.Client
	armed   atomic.Bool
	deleted chan *apiv2.PhysicalContainer
	release <-chan struct{}
}

func (client *delayedTunnelContainerDeletion) Delete(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.DeleteOption) error {
	physical, isPhysical := object.(*apiv2.PhysicalContainer)
	if !isPhysical || !client.armed.CompareAndSwap(true, false) {
		return client.Client.Delete(ctx, object, options...)
	}
	deleteErr := client.Client.Delete(ctx, object, options...)
	if deleteErr != nil {
		return deleteErr
	}
	select {
	case client.deleted <- physical.DeepCopy():
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-client.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type delayedTunnelExitExecutor struct {
	process.Executor
	first   sync.Once
	delayed chan<- func()
}

func (executor *delayedTunnelExitExecutor) StartProcess(
	ctx context.Context,
	command *exec.Cmd,
	handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag,
	create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	delay := false
	executor.first.Do(func() { delay = true })
	if delay {
		originalHandler := handler
		handler = process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, exitErr error) {
			executor.delayed <- func() { originalHandler.OnProcessExited(pid, exitCode, exitErr) }
		})
	}
	return executor.Executor.StartProcess(ctx, command, handler, flags, create)
}

func createRunningTunnelProxy(
	t *testing.T,
	ctx context.Context,
	name string,
	options TestEnvironmentOptions,
) (*ctrl_testutil.ApiServerInfo, *TestEnvironmentInfo, *apiv1.ContainerNetworkTunnelProxy) {
	t.Helper()
	dcppaths.EnableTestPathProbing()
	server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir(), options)
	require.NoError(t, startErr)
	simulateServerProxy(t, 16783, environment.TestProcessExecutor)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: name + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	resource := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{Name: "not-ready", ServerServiceName: "missing-server", ClientServiceName: "missing-client"},
			},
		},
	}
	require.NoError(t, server.Client.Create(ctx, resource))
	running := waitAllTunnelsInState(t, ctx, server.Client, resource.NamespacedName(), 1, apiv1.TunnelStateNotReady)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, running.Status.State)
	return server, environment, running
}

// Verifies that manageTunnelProxy and handleDeletionRequest safely initialize missing
// in-memory state regardless of persisted Status.State, transitioning unowned active
// states to Failed without panicking and completing deletion cleanup without deadlocking.
func TestTunnelProxyMissingInMemoryStateFailsOrDeletesSafely(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	holdGate := &tunnelProxyHoldByNameClient{}
	server, environment, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				holdGate.Client = client
				return holdGate
			},
		},
	)
	require.NoError(t, startErr)
	simulateServerProxy(t, 16790, environment.TestProcessExecutor)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "missing-pd-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	require.NoError(t, controllers.EnsureV1PhysicalResourcesNamespace(ctx, server.Client))

	finalizerName := fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)

	// Every non-Empty/Pending persisted state must enter Failed when pd == nil.
	for i, persistedState := range []apiv1.ContainerNetworkTunnelProxyState{
		apiv1.ContainerNetworkTunnelProxyStateBuildingImage,
		apiv1.ContainerNetworkTunnelProxyStateStarting,
		apiv1.ContainerNetworkTunnelProxyStateStarted,
		apiv1.ContainerNetworkTunnelProxyStateRunning,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryPending,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryWait,
		apiv1.ContainerNetworkTunnelProxyStateFailed,
	} {
		proxyName := fmt.Sprintf("missing-pd-active-%d", i)
		holdGate.hold(proxyName)
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:       proxyName,
				Finalizers: []string{finalizerName},
			},
			Spec: apiv1.ContainerNetworkTunnelProxySpec{
				ContainerNetworkName: network.Name,
			},
		}
		require.NoError(t, server.Client.Create(ctx, proxy))
		require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
			current.Status.State = persistedState
			return server.Client.Status().Update(updateCtx, current)
		}))
		holdGate.releaseWhenStateObserved(proxyName, persistedState, false)

		failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
			return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
		})
		require.NoError(t, server.Client.Delete(ctx, failed))
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, failed)
	}

	// Deletion with pd == nil and persisted Deleting/Deleted/RecoveryWait
	// must run cleanup of owned physical containers and remove the finalizer without wedging.
	for i, persistedDeletionState := range []apiv1.ContainerNetworkTunnelProxyState{
		apiv1.ContainerNetworkTunnelProxyStateDeleting,
		apiv1.ContainerNetworkTunnelProxyStateDeleted,
		apiv1.ContainerNetworkTunnelProxyStateRecoveryWait,
	} {
		proxyName := fmt.Sprintf("missing-pd-delete-%d", i)
		holdGate.hold(proxyName)
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:       proxyName,
				Finalizers: []string{finalizerName},
			},
			Spec: apiv1.ContainerNetworkTunnelProxySpec{
				ContainerNetworkName: network.Name,
			},
		}
		require.NoError(t, server.Client.Create(ctx, proxy))

		physicalName := types.NamespacedName{
			Namespace: controllers.V1PhysicalResourcesNamespaceName,
			Name:      fmt.Sprintf("tunnel-proxy-%s", proxy.UID),
		}
		physicalContainer := &apiv2.PhysicalContainer{
			ObjectMeta: metav1.ObjectMeta{
				Name:      physicalName.Name,
				Namespace: physicalName.Namespace,
				Annotations: map[string]string{
					"container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-name":      proxy.Name,
					"container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-namespace": proxy.Namespace,
					"container-network-tunnel-proxy.usvc-dev.developer.microsoft.com/owner-uid":       string(proxy.UID),
				},
			},
			Spec: apiv2.PhysicalContainerSpec{
				Container: &apiv2.PhysicalContainerConfig{
					ImageRef:      controllers.V1TunnelProxyPhysicalContainerImageName,
					ContainerName: physicalName.Name,
				},
			},
		}
		require.NoError(t, server.Client.Create(ctx, physicalContainer))

		require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
			current.Status.State = persistedDeletionState
			return server.Client.Status().Update(updateCtx, current)
		}))
		require.NoError(t, server.Client.Delete(ctx, proxy))
		holdGate.releaseWhenStateObserved(proxyName, persistedDeletionState, true)

		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physicalContainer)
	}
}

type tunnelProxyHoldRule struct {
	held              bool
	releaseOnObserved bool
	expectedState     apiv1.ContainerNetworkTunnelProxyState
	requireDeleting   bool
}

type tunnelProxyHoldByNameClient struct {
	ctrl_client.Client
	lock  sync.Mutex
	rules map[string]tunnelProxyHoldRule
}

func (c *tunnelProxyHoldByNameClient) hold(name string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.rules == nil {
		c.rules = make(map[string]tunnelProxyHoldRule)
	}
	c.rules[name] = tunnelProxyHoldRule{held: true}
}

func (c *tunnelProxyHoldByNameClient) releaseWhenStateObserved(
	name string,
	expectedState apiv1.ContainerNetworkTunnelProxyState,
	requireDeleting bool,
) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.rules[name] = tunnelProxyHoldRule{
		held:              true,
		releaseOnObserved: true,
		expectedState:     expectedState,
		requireDeleting:   requireDeleting,
	}
}

func (c *tunnelProxyHoldByNameClient) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	getErr := c.Client.Get(ctx, key, object, options...)
	if getErr != nil {
		return getErr
	}
	proxy, isProxy := object.(*apiv1.ContainerNetworkTunnelProxy)
	if !isProxy {
		return nil
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	rule, exists := c.rules[key.Name]
	if !exists || !rule.held {
		return nil
	}
	if rule.releaseOnObserved &&
		proxy.Status.State == rule.expectedState &&
		(!rule.requireDeleting || !proxy.DeletionTimestamp.IsZero()) {
		delete(c.rules, key.Name)
		return nil
	}
	return errors.New("holding initial reconciliation until test state is visible in informer cache")
}

// Verifies that cancelling controller lifetime during server startup or shared image creation
// uses the work-queue drain context to stop the server process cleanly and remove unused build-context archives.
func TestTunnelProxyShutdownDrainStopsStartingServerAndCleansImageArchive(t *testing.T) {
	t.Parallel()

	t.Run("server-startup-stop-uses-live-drain-context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		dcppaths.EnableTestPathProbing()

		runCtx, stopRun := context.WithCancel(ctx)
		defer stopRun()

		var stopContextLock sync.Mutex
		var stopContextErrs []error
		server, environment, startErr := StartTestEnvironmentWithOptions(
			t,
			runCtx,
			ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
			t.Name(),
			t.TempDir(),
			TestEnvironmentOptions{
				ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
					config.ProcessExecutor = &stopContextRecordingExecutor{
						Executor: config.ProcessExecutor,
						record: func(stopCtxErr error) {
							stopContextLock.Lock()
							stopContextErrs = append(stopContextErrs, stopCtxErr)
							stopContextLock.Unlock()
						},
					}
				},
			},
		)
		require.NoError(t, startErr)

		dcpPath, pathErr := dcppaths.GetDcpExePath()
		require.NoError(t, pathErr)
		configWritten := concurrency.NewValuePromise[bool]()
		environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
			Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
			RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
				tcBytes, marshalErr := json.Marshal(dcptun.TunnelProxyConfig{ServerControlPort: 16791})
				if marshalErr != nil {
					t.Errorf("Could not marshal TunnelProxyConfig: %v", marshalErr)
					return 1
				}
				if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(tcBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
					t.Errorf("Could not write TunnelProxyConfig to stdout: %v", writeErr)
					return 1
				}
				configWritten.Set(true)
				// Block without writing readiness until stopped during shutdown drain.
				select {
				case <-pe.Signal:
				case <-ctx.Done():
				}
				return 0
			},
		})

		network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "drain-server-stop-network"}}
		require.NoError(t, server.Client.Create(ctx, network))
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "drain-server-stop-proxy"},
			Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
		}
		require.NoError(t, server.Client.Create(ctx, proxy))

		select {
		case <-configWritten.Wait():
		case <-ctx.Done():
			t.Fatal("Server proxy did not reach readiness wait")
		}

		stopRun()
		select {
		case <-server.ApiServerDisposalComplete.Wait():
		case <-ctx.Done():
			t.Fatal("Test environment did not finish shutdown")
		}

		stopContextLock.Lock()
		recorded := std_slices.Clone(stopContextErrs)
		stopContextLock.Unlock()
		require.Len(t, recorded, 1, "cleanupFailedStart must stop the process once without falling back to a second stop")
		require.NoError(t, recorded[0], "StopProcess context during shutdown drain must not be pre-canceled")
	})

	t.Run("image-create-cancellation-removes-build-context-archive", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		dcppaths.EnableTestPathProbing()

		runCtx, stopRun := context.WithCancel(ctx)
		defer stopRun()

		imageCreateEntered := concurrency.NewValuePromise[string]()
		releaseImageCreate := make(chan struct{})
		closeRelease := sync.OnceFunc(func() { close(releaseImageCreate) })
		defer closeRelease()

		server, environment, startErr := StartTestEnvironmentWithOptions(
			t,
			runCtx,
			ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
			t.Name(),
			t.TempDir(),
			TestEnvironmentOptions{
				DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
					return &imageCreateCancelClient{
						Client:  client,
						entered: imageCreateEntered,
						release: releaseImageCreate,
					}
				},
			},
		)
		require.NoError(t, startErr)
		simulateServerProxy(t, 16792, environment.TestProcessExecutor)

		network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "drain-image-archive-network"}}
		require.NoError(t, server.Client.Create(ctx, network))
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "drain-image-archive-proxy"},
			Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
		}
		require.NoError(t, server.Client.Create(ctx, proxy))

		var archivePath string
		select {
		case <-imageCreateEntered.Wait():
			archivePath = imageCreateEntered.Get()
		case <-ctx.Done():
			t.Fatal("PhysicalContainerImage Create was not attempted")
		}
		require.FileExists(t, archivePath)

		stopRun()
		closeRelease()

		select {
		case <-server.ApiServerDisposalComplete.Wait():
		case <-ctx.Done():
			t.Fatal("Test environment did not finish shutdown")
		}
		require.NoFileExists(t, archivePath, "Unused build-context archive must be removed during shutdown drain")
	})
}

type stopContextRecordingExecutor struct {
	process.Executor
	record func(error)
}

func (e *stopContextRecordingExecutor) StopProcess(
	ctx context.Context,
	handle process.ProcessHandle,
	options ...process.ProcessStopOption,
) error {
	e.record(ctx.Err())
	return e.Executor.StopProcess(ctx, handle, options...)
}

type imageCreateCancelClient struct {
	ctrl_client.Client
	entered *concurrency.ValuePromise[string]
	release <-chan struct{}
}

func (c *imageCreateCancelClient) Create(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.CreateOption,
) error {
	if image, isImage := object.(*apiv2.PhysicalContainerImage); isImage {
		archiveSource := image.Spec.Image.Build.ContextArchive.Source
		c.entered.Set(archiveSource)
		<-c.release
		return context.Canceled
	}
	return c.Client.Create(ctx, object, options...)
}

// Verifies that an immediate tunnel-server exit before printing configuration fails the proxy
// without waiting for the server configuration read timeout to elapse.
func TestTunnelProxyServerImmediateExitFailsConfigReadPromptly(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	server, environment, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				// Far longer than the test, so reaching Failed proves the process exit ended the configuration read.
				config.ServerConfigReadTimeout = 10 * time.Minute
			},
		},
	)
	require.NoError(t, startErr)

	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 {
			return 2
		},
	})

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "immediate-exit-config-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "immediate-exit-config-proxy"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))

	failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})
	require.Contains(t, failed.Status.Message, "Failed to read server proxy configuration:")
	require.Contains(t, failed.Status.Message, "exited with exit code 2 before writing configuration")
}

// Verifies that a Failed ContainerNetworkTunnelProxy projects its in-memory tunnel statuses onto the API
// object and does not adopt tunnel statuses written to the API by another client.
func TestTunnelProxyFailedStateProjectsInMemoryTunnelStatuses(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "failed-state-projection"

	server, environment, startErr := StartTestEnvironment(t, ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController, t.Name(), t.TempDir())
	require.NoError(t, startErr)
	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition:  internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
		RunCommand: func(*internal_testutil.ProcessExecution) int32 { return 2 },
	})

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})
	require.NotEmpty(t, failed.Status.Message)

	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Status.Message = "externally modified status"
		current.Status.TunnelStatuses = []apiv1.TunnelStatus{{
			Name:      "externally-added-tunnel",
			State:     apiv1.TunnelStateNotReady,
			Timestamp: metav1.NewMicroTime(time.Now()),
		}}
		return server.Client.Status().Update(updateCtx, current)
	}))

	// The controller restores the message and the tunnel statuses in the same projection.
	projected := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.Message == failed.Status.Message, nil
	})
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateFailed, projected.Status.State)
	require.Empty(t, projected.Status.TunnelStatuses, "A Failed proxy must not adopt tunnel statuses written by another client")
}

// Verifies that two tunnels sharing a ServerService receive distinct TunnelIDs even after
// a lost PrepareTunnel response, and that re-adding a tunnel while its previous incarnation
// is Deleting releases its old publishedPorts reservation.
func TestTunnelProxySharedServerServiceAndReaddedDeletingTunnel(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "shared-server-and-readd"

	blockDeletion := &atomic.Bool{}
	var deleteAttempts atomic.Uint32
	server, environment, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(connection grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					return &tunnelDeletionObserver{
						TunnelControlClient: makeClient(connection),
						blocked:             blockDeletion,
						attempts:            &deleteAttempts,
					}
				}
			},
		},
	)
	require.NoError(t, startErr)
	simulateServerProxy(t, 16793, environment.TestProcessExecutor)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))

	// Create shared ServerService (port 30500) and a second ServerService (port 30600) for re-adding.
	sharedTunnels := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 1, testName+"-shared", 30500)
	secondServerTunnels := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 1, testName+"-second", 30600)

	clientServiceB := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testName + "-client-b"},
		Spec: apiv1.ServiceSpec{
			Protocol:              commonapi.PortProtocolTCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}
	require.NoError(t, server.Client.Create(ctx, clientServiceB))

	// Also enable random-port fallback template for the shared ServerService so the second tunnel can bind an ephemeral port.
	environment.TestTunnelControlClient.EnableTunnel(&dcptunproto.TunnelSpec{
		ServerAddress: stdproto.String(networking.IPv4LocalhostDefaultAddress),
		ServerPort:    stdproto.Int32(30500),
	})
	// Lose the first PrepareTunnel response on the shared ServerService.
	environment.TestTunnelControlClient.FailNextPrepareResponse(
		sharedTunnels[0].fingerprint,
		status.Error(codes.DeadlineExceeded, "simulated lost PrepareTunnel response"),
	)

	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              "tunnel-a",
					ServerServiceName: sharedTunnels[0].serverServiceName,
					ClientServiceName: sharedTunnels[0].clientServiceName,
				},
				{
					Name:              "tunnel-b",
					ServerServiceName: sharedTunnels[0].serverServiceName,
					ClientServiceName: clientServiceB.Name,
				},
			},
		},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))

	readyProxy := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 2, apiv1.TunnelStateReady)
	statuses := maps.SliceToMap(readyProxy.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.NotEqual(t, statuses["tunnel-a"].TunnelID, statuses["tunnel-b"].TunnelID, "Distinct tunnels sharing a ServerService must not share a TunnelID")
	require.NotEqual(t, statuses["tunnel-a"].ClientProxyPort, statuses["tunnel-b"].ClientProxyPort)

	// Block DeleteTunnel and remove tunnel-a so it enters TunnelStateDeleting.
	oldTunnelBID := statuses["tunnel-b"].TunnelID
	blockDeletion.Store(true)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = []apiv1.TunnelConfiguration{
			{
				Name:              "tunnel-b",
				ServerServiceName: sharedTunnels[0].serverServiceName,
				ClientServiceName: clientServiceB.Name,
			},
		}
		return server.Client.Update(updateCtx, current)
	}))

	_ = waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		byName := maps.SliceToMap(current.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
		deletingStatus, hasA := byName["tunnel-a"]
		return hasA && deletingStatus.State == apiv1.TunnelStateDeleting && deleteAttempts.Load() >= 1, nil
	})

	// Re-add tunnel-a targeting secondServerTunnels[0] (port 30600) while tunnel-a is still Deleting, then unblock deletion.
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = append(current.Spec.Tunnels, apiv1.TunnelConfiguration{
			Name:              "tunnel-a",
			ServerServiceName: secondServerTunnels[0].serverServiceName,
			ClientServiceName: secondServerTunnels[0].clientServiceName,
		})
		return server.Client.Update(updateCtx, current)
	}))
	blockDeletion.Store(false)

	readdedReady := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		if len(current.Status.TunnelStatuses) != 2 {
			return false, nil
		}
		byName := maps.SliceToMap(current.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
		return byName["tunnel-a"].State == apiv1.TunnelStateReady &&
			byName["tunnel-a"].ClientProxyPort == 30600 &&
			byName["tunnel-b"].State == apiv1.TunnelStateReady, nil
	})
	readdedStatuses := maps.SliceToMap(readdedReady.Status.TunnelStatuses, apiv1.TunnelStatus.KV)
	require.Equal(t, int32(30600), readdedStatuses["tunnel-a"].ClientProxyPort, "Re-added tunnel must not retain deleted tunnel's publishedPorts reservation")
	acceptingB, foundB := environment.TestTunnelControlClient.TunnelAccepting(oldTunnelBID)
	require.True(t, foundB && acceptingB, "Deleting tunnel-a must not tear down tunnel-b")
}

// Verifies that deleteTunnel evicts workloadEndpointCache and that RecoveryCleanup retries
// failed Endpoint deletions before advancing to RecoveryWait so tunnels removed during
// recovery never leave orphaned Endpoints behind.
func TestTunnelProxyEndpointCacheEvictionAndRecoveryWithdrawalRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "endpoint-cache-and-recovery-retry"

	endpointFaults := &selectiveEndpointDeleteFaultClient{}
	server, environment, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				endpointFaults.Client = client
				return endpointFaults
			},
		},
	)
	require.NoError(t, startErr)
	simulateServerProxy(t, 16794, environment.TestProcessExecutor)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, server.Client.Create(ctx, network))

	tunnels := prepareTunnelServices(t, ctx, server.Client, environment.TestTunnelControlClient, 2, testName, 30700)
	// Hide tunnels[1]'s Endpoints from informer List on the controller client so workloadEndpointCache
	// is authoritative whenever present.
	endpointFaults.hideServiceInList.Store(tunnels[1].clientServiceName)

	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              tunnels[0].tunnelName,
					ServerServiceName: tunnels[0].serverServiceName,
					ClientServiceName: tunnels[0].clientServiceName,
				},
				{
					Name:              tunnels[1].tunnelName,
					ServerServiceName: tunnels[1].serverServiceName,
					ClientServiceName: tunnels[1].clientServiceName,
				},
			},
		},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))

	_ = waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 2, apiv1.TunnelStateReady)
	require.GreaterOrEqual(t, endpointFaults.endpointsAtFirstReadyPatch.Load(), int32(2),
		"Endpoints must be created before Status is patched to TunnelStateReady in the same reconciliation pass")
	waitEndpointCountEx(t, ctx, server.Client, tunnels[0].clientServiceName, 1)
	waitEndpointCountEx(t, ctx, server.Client, tunnels[1].clientServiceName, 1)

	// Remove tunnels[1] while informer List hides its Endpoints, then re-add it.
	// Without workloadEndpointCache eviction in deleteTunnel, re-adding tunnels[1] would trust the stale
	// cached Endpoint (matching TunnelID/Address/Port) and never recreate the deleted Endpoint.
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = current.Spec.Tunnels[:1]
		return server.Client.Update(updateCtx, current)
	}))
	_ = waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	waitEndpointCountEx(t, ctx, server.Client, tunnels[1].clientServiceName, 0)

	// Reset tunnel ID counter to 1 on tunnels[1]'s template isn't even needed: any stale cache entry with the old TunnelID
	// or a matching port/ID is invalidated; verify re-adding tunnels[1] recreates its Endpoint cleanly.
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = append(current.Spec.Tunnels, apiv1.TunnelConfiguration{
			Name:              tunnels[1].tunnelName,
			ServerServiceName: tunnels[1].serverServiceName,
			ClientServiceName: tunnels[1].clientServiceName,
		})
		return server.Client.Update(updateCtx, current)
	}))
	runningBeforeCrash := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 2, apiv1.TunnelStateReady)
	waitEndpointCountEx(t, ctx, server.Client, tunnels[1].clientServiceName, 1)

	// Unhide List, fail Endpoint Delete, trigger recovery, remove tunnels[1], then restore Delete.
	endpointFaults.hideServiceInList.Store("")
	endpointFaults.failEndpointDeletes.Store(true)

	serverPID, pidErr := process.Int64_ToPidT(*runningBeforeCrash.Status.ServerProxyProcessID)
	require.NoError(t, pidErr)
	environment.TestProcessExecutor.SimulateProcessExit(t, serverPID, 0)

	// Every cleanup pass withdraws the Endpoints tracked for each tunnel before listing the remaining ones,
	// so a second listing after a failed deletion means a cleanup pass that failed to remove Endpoints was retried.
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return endpointFaults.listingsAfterFailedDelete.Load() >= 2, nil
	}))
	var inCleanup apiv1.ContainerNetworkTunnelProxy
	require.NoError(t, server.Client.Get(ctx, proxy.NamespacedName(), &inCleanup))
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup, inCleanup.Status.State,
		"Recovery must not advance while Endpoint withdrawal fails")
	replacementContainerName := types.NamespacedName{
		Namespace: controllers.V1PhysicalResourcesNamespaceName,
		Name:      fmt.Sprintf("tunnel-proxy-%s-1", proxy.UID),
	}
	replacementContainerErr := server.Client.Get(ctx, replacementContainerName, &apiv2.PhysicalContainer{})
	require.True(t, apimachinery_errors.IsNotFound(replacementContainerErr),
		"The replacement pair must not start while Endpoint withdrawal fails: %v", replacementContainerErr)

	// Remove tunnels[1] while recovery cleanup is still retrying Endpoint withdrawal, then restore API deletes.
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = current.Spec.Tunnels[:1]
		return server.Client.Update(updateCtx, current)
	}))
	endpointFaults.failEndpointDeletes.Store(false)

	recovered := waitAllTunnelsInState(t, ctx, server.Client, proxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, recovered.Status.State)
	waitEndpointCountEx(t, ctx, server.Client, tunnels[0].clientServiceName, 1)
	waitEndpointCountEx(t, ctx, server.Client, tunnels[1].clientServiceName, 0)
	_ = waitObjectAssumesStateEx(t, ctx, server.Client, tunnels[1].clientServiceNamespacedName, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateNotReady, nil
	})

	// Deleting the running ContainerNetworkTunnelProxy must also withdraw its remaining client Endpoints.
	require.NoError(t, server.Client.Delete(ctx, recovered))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, recovered)
	waitEndpointCountEx(t, ctx, server.Client, tunnels[0].clientServiceName, 0)
	_ = waitObjectAssumesStateEx(t, ctx, server.Client, tunnels[0].clientServiceNamespacedName, func(svc *apiv1.Service) (bool, error) {
		return svc.Status.State == apiv1.ServiceStateNotReady, nil
	})
}

type selectiveEndpointDeleteFaultClient struct {
	ctrl_client.Client
	hideServiceInList          atomic.Value // string
	failEndpointDeletes        atomic.Bool
	failedDeletes              atomic.Uint32
	listingsAfterFailedDelete  atomic.Uint32
	endpointsCreated           atomic.Int32
	firstReadyPatchRecorded    atomic.Bool
	endpointsAtFirstReadyPatch atomic.Int32
}

func (c *selectiveEndpointDeleteFaultClient) Create(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.CreateOption,
) error {
	createErr := c.Client.Create(ctx, object, options...)
	if createErr == nil {
		if _, isEndpoint := object.(*apiv1.Endpoint); isEndpoint {
			c.endpointsCreated.Add(1)
		}
	}
	return createErr
}

func (c *selectiveEndpointDeleteFaultClient) Status() ctrl_client.SubResourceWriter {
	return &selectiveEndpointStatusWriter{
		SubResourceWriter: c.Client.Status(),
		parent:            c,
	}
}

type selectiveEndpointStatusWriter struct {
	ctrl_client.SubResourceWriter
	parent *selectiveEndpointDeleteFaultClient
}

func (w *selectiveEndpointStatusWriter) Patch(
	ctx context.Context,
	object ctrl_client.Object,
	patch ctrl_client.Patch,
	options ...ctrl_client.SubResourcePatchOption,
) error {
	if proxy, isProxy := object.(*apiv1.ContainerNetworkTunnelProxy); isProxy {
		allReady := len(proxy.Status.TunnelStatuses) == 2 &&
			slices.All(proxy.Status.TunnelStatuses, func(ts apiv1.TunnelStatus) bool {
				return ts.State == apiv1.TunnelStateReady
			})
		if allReady && w.parent.firstReadyPatchRecorded.CompareAndSwap(false, true) {
			w.parent.endpointsAtFirstReadyPatch.Store(w.parent.endpointsCreated.Load())
		}
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func (c *selectiveEndpointDeleteFaultClient) List(
	ctx context.Context,
	list ctrl_client.ObjectList,
	options ...ctrl_client.ListOption,
) error {
	listErr := c.Client.List(ctx, list, options...)
	if listErr != nil {
		return listErr
	}
	endpoints, isEndpointList := list.(*apiv1.EndpointList)
	if !isEndpointList {
		return nil
	}
	if c.failEndpointDeletes.Load() && c.failedDeletes.Load() > 0 {
		c.listingsAfterFailedDelete.Add(1)
	}
	hiddenService, _ := c.hideServiceInList.Load().(string)
	if hiddenService == "" {
		return nil
	}
	filtered := endpoints.Items[:0]
	for _, endpoint := range endpoints.Items {
		if endpoint.Spec.ServiceName != hiddenService {
			filtered = append(filtered, endpoint)
		}
	}
	endpoints.Items = filtered
	return nil
}

func (c *selectiveEndpointDeleteFaultClient) Delete(
	ctx context.Context,
	object ctrl_client.Object,
	options ...ctrl_client.DeleteOption,
) error {
	if _, isEndpoint := object.(*apiv1.Endpoint); isEndpoint && c.failEndpointDeletes.Load() {
		c.failedDeletes.Add(1)
		return errors.New("simulated transient Endpoint deletion failure")
	}
	return c.Client.Delete(ctx, object, options...)
}

// Verifies that per-reconcile context expiration during createProxyControlSecurityConfig in BuildingImage
// requeues reconciliation rather than permanently marking the proxy Failed in memory.
func TestTunnelProxyCanceledReconcileDuringSecurityConfigDoesNotFail(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	imageTimeoutClient := &oneShotImageGetTimeoutClient{}
	server, environment, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateTunnelProxyClient: func(client ctrl_client.Client) ctrl_client.Client {
				imageTimeoutClient.Client = client
				return imageTimeoutClient
			},
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				config.ReconciliationTimeout = 5 * time.Second
			},
		},
	)
	require.NoError(t, startErr)
	simulateServerProxy(t, 16795, environment.TestProcessExecutor)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "canceled-sec-config-network"}}
	require.NoError(t, server.Client.Create(ctx, network))

	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "canceled-sec-config-proxy"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, server.Client.Create(ctx, proxy))

	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		require.NotEqual(t, apiv1.ContainerNetworkTunnelProxyStateFailed, current.Status.State,
			"Context expiration during security config creation must not transition proxy to Failed")
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
	require.Equal(t, apiv1.ContainerNetworkTunnelProxyStateRunning, running.Status.State)
	require.True(t, imageTimeoutClient.expiredObserved.Load(), "Test must observe per-reconcile context expiration before createProxyControlSecurityConfig")
}

type oneShotImageGetTimeoutClient struct {
	ctrl_client.Client
	triggered       atomic.Bool
	expiredObserved atomic.Bool
}

func (c *oneShotImageGetTimeoutClient) Get(
	ctx context.Context,
	key ctrl_client.ObjectKey,
	object ctrl_client.Object,
	options ...ctrl_client.GetOption,
) error {
	getErr := c.Client.Get(context.WithoutCancel(ctx), key, object, options...)
	if getErr != nil {
		return getErr
	}
	image, isImage := object.(*apiv2.PhysicalContainerImage)
	if isImage && image.Status.Phase == apiv2.PhysicalContainerImagePhaseReady && c.triggered.CompareAndSwap(false, true) {
		// Wait until this single reconciliation's ReconciliationTimeout expires so the immediately
		// following createProxyControlSecurityConfig(ctx, ...) call receives ctx.Err() != nil.
		<-ctx.Done()
		c.expiredObserved.Store(true)
	}
	return nil
}

// Verifies that ContainerNetworkTunnelProxy restores real container traffic after
// server loss and client removal while retaining the consumer and its random fallback port.
func TestTunnelProxyRecoveryWithRealOrchestrator(t *testing.T) {
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)
	ctx, cancel := testutil.GetTestContext(t, 3*time.Minute)
	t.Cleanup(cancel)
	listener, listenErr := net.Listen("tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, dcptun.DefaultContainerProxyControlPort))
	if listenErr != nil {
		t.Skipf("host port %d is needed to force a client-proxy port collision: %v", dcptun.DefaultContainerProxyControlPort, listenErr)
	}
	echoCtx, stopEcho := context.WithCancel(ctx)
	var echoWorkers sync.WaitGroup
	t.Cleanup(func() {
		stopEcho()
		require.NoError(t, listener.Close())
		echoWorkers.Wait()
	})
	echoWorkers.Go(func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				if !errors.Is(acceptErr, net.ErrClosed) {
					t.Errorf("echo listener failed: %v", acceptErr)
				}
				return
			}
			echoWorkers.Go(func() {
				defer connection.Close()
				stopClose := context.AfterFunc(echoCtx, func() { _ = connection.Close() })
				defer stopClose()
				_, _ = io.Copy(connection, connection)
			})
		}
	})
	containertest.ForEachHealthyRuntime(t, ctx, func(t *testing.T, runtimeCtx context.Context, runtime containertest.Runtime) {
		runCtx, stopRun := context.WithCancel(runtimeCtx)
		defer stopRun()
		tracker := containertest.NewResourceTracker(t, runtime)
		server, environment, startErr := StartAdvancedTestEnvironmentWithOptions(runCtx, AllControllers,
			containertest.UniqueName(t, "tunnel-recovery"), t.TempDir(), AdvancedTestEnvironmentOptions{
				ApiServerFlags: ctrl_testutil.ApiServerUseTrueContainerOrchestrator, ContainerOrchestrator: runtime.Orchestrator,
			})
		require.NoError(t, startErr)
		defer environment.ProcessExecutor.Dispose()
		defer shutdownAdvancedTestEnvironment(t, runCtx, stopRun, server)
		serverService := &apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-server"},
			Spec:       apiv1.ServiceSpec{Protocol: commonapi.PortProtocolTCP, AddressAllocationMode: apiv1.AddressAllocationModeProxyless},
		}
		clientService := &apiv1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-client"},
			Spec:       apiv1.ServiceSpec{Protocol: commonapi.PortProtocolTCP, AddressAllocationMode: apiv1.AddressAllocationModeProxyless},
		}
		require.NoError(t, server.Client.Create(runCtx, serverService))
		require.NoError(t, server.Client.Create(runCtx, clientService))
		endpoint := &apiv1.Endpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-server-endpoint"},
			Spec: apiv1.EndpointSpec{
				ServiceName: serverService.Name, Address: networking.IPv4LocalhostDefaultAddress, Port: dcptun.DefaultContainerProxyControlPort,
			},
		}
		require.NoError(t, server.Client.Create(runCtx, endpoint))
		network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "recovery-network"}}
		require.NoError(t, server.Client.Create(runCtx, network))
		const proxyAlias = "recovery-host"
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-proxy"},
			Spec: apiv1.ContainerNetworkTunnelProxySpec{
				ContainerNetworkName: network.Name, Aliases: []string{proxyAlias},
				Tunnels: []apiv1.TunnelConfiguration{{Name: "echo", ServerServiceName: serverService.Name, ClientServiceName: clientService.Name}},
			},
		}
		require.NoError(t, server.Client.Create(runCtx, proxy))
		running := waitAllTunnelsInState(t, runCtx, server.Client, proxy.NamespacedName(), 1, apiv1.TunnelStateReady)
		publishedPort := running.Status.TunnelStatuses[0].ClientProxyPort
		require.NotEqual(t, int32(dcptun.DefaultContainerProxyControlPort), publishedPort, "The occupied client port must cause a real random-port fallback")
		toolDir, toolErr := internal_testutil.GetTestToolDir("parrot")
		require.NoError(t, toolErr)
		imageName := "localhost/" + containertest.UniqueName(t, "tunnel-recovery-client") + ":latest"
		require.NoError(t, tracker.TrackImage(imageName))
		image, binaryPath, imageErr := ensureParrotContainerImage(runCtx, filepath.Join(toolDir, "parrot_c"), imageName, tracker.MapLabels(), runtime.Orchestrator)
		require.NoError(t, imageErr)
		consumer := &apiv1.Container{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-consumer"},
			Spec: apiv1.ContainerSpec{
				Image: image, Command: binaryPath,
				Args: []string{"server", "--port", "0", "--timeout", "3m"},
				Networks: &[]apiv1.ContainerNetworkConnectionConfig{
					{Name: network.Name},
				},
			},
		}
		require.NoError(t, server.Client.Create(runCtx, consumer))
		originalConsumer := waitObjectAssumesStateEx(t, runCtx, server.Client, consumer.NamespacedName(), func(current *apiv1.Container) (bool, error) {
			return current.Status.State == apiv1.ContainerStateRunning, nil
		})
		probe := func() {
			var stdout, stderr bytes.Buffer
			exitCodes, execErr := runtime.Orchestrator.ExecContainer(runCtx, containers.ExecContainerOptions{
				Container: originalConsumer.Status.ContainerID, Command: binaryPath,
				Args: []string{"client", "--address", proxyAlias, "--port", fmt.Sprintf("%d", publishedPort), "--conversations", "1", "--timeout", "20s"},
				StreamCommandOptions: containers.StreamCommandOptions{
					StdOutStream: usvc_io.NopWriteCloser(&stdout), StdErrStream: usvc_io.NopWriteCloser(&stderr),
				},
			})
			require.NoError(t, execErr)
			select {
			case exitCode, open := <-exitCodes:
				require.True(t, open, "probe execution must report its exit code")
				require.Zero(t, exitCode, "Container probe failed: %s %s", stdout.String(), stderr.String())
			case <-runCtx.Done():
				t.Fatal("Container traffic did not complete before test cancellation")
			}
		}
		probe()
		for _, fault := range []string{"server", "client"} {
			previous := running
			if fault == "server" {
				pid, pidErr := process.Int64_ToPidT(*previous.Status.ServerProxyProcessID)
				require.NoError(t, pidErr)
				handle := process.NewHandle(pid, previous.Status.ServerProxyStartupTimestamp.Time)
				require.NoError(t, environment.ProcessExecutor.StopProcess(runCtx, handle))
			} else {
				_, removeErr := runtime.Orchestrator.RemoveContainers(runCtx, containers.RemoveContainersOptions{
					Containers: []string{previous.Status.ClientProxyContainerID}, Force: true,
				})
				require.NoError(t, removeErr)
			}
			running = waitObjectAssumesStateEx(t, runCtx, server.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
				return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning &&
					current.Status.ServerProxyProcessID != nil &&
					*current.Status.ServerProxyProcessID != *previous.Status.ServerProxyProcessID &&
					current.Status.ClientProxyContainerID != previous.Status.ClientProxyContainerID &&
					len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv1.TunnelStateReady, nil
			})
			require.Equal(t, publishedPort, running.Status.TunnelStatuses[0].ClientProxyPort)
			require.Equal(t, proxy.UID, running.UID)
			probe()
			currentConsumer := &apiv1.Container{}
			require.NoError(t, server.Client.Get(runCtx, consumer.NamespacedName(), currentConsumer))
			require.Equal(t, originalConsumer.UID, currentConsumer.UID)
			require.Equal(t, originalConsumer.Status.ContainerID, currentConsumer.Status.ContainerID)
			t.Logf("Recovered from %s loss with consumer %s and published fallback port %d unchanged", fault, currentConsumer.Status.ContainerID, publishedPort)
		}
	})
}

// Verifies that a ContainerNetworkTunnelProxy works with each supported real container orchestrator,
// and that client proxy ports remain loopback-only while private credentials are runtime-injected rather than image-baked.
// This is an advanced test that is not included in routine test runs.
// Requires DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR environment variable to be set to "true".
func TestTunnelProxyWithRealOrchestrator(t *testing.T) {
	t.Parallel()

	const testTimeout = 3 * time.Minute
	testCtx, testCancel := testutil.GetTestContext(t, testTimeout)
	t.Cleanup(testCancel)

	containertest.ForEachHealthyRuntime(t, testCtx, testTunnelProxyWithRealOrchestrator)
}

func testTunnelProxyWithRealOrchestrator(
	t *testing.T,
	runtimeCtx context.Context,
	runtime containertest.Runtime,
) {
	const parrotTimeout = 3 * time.Minute

	ctx, cancel := context.WithCancel(runtimeCtx)
	defer cancel()
	testName := containertest.UniqueName(t, "test-tunnel-proxy-with-real-orchestrator")
	resourceTracker := containertest.NewResourceTracker(t, runtime)
	dcppaths.EnableTestPathProbing()

	serverInfo, teInfo, startupErr := StartAdvancedTestEnvironmentWithOptions(
		ctx,
		AllControllers,
		testName,
		t.TempDir(),
		AdvancedTestEnvironmentOptions{
			ApiServerFlags:        ctrl_testutil.ApiServerUseTrueContainerOrchestrator,
			ContainerOrchestrator: runtime.Orchestrator,
		},
	)
	require.NoError(t, startupErr, "Failed to start the API server")
	t.Logf("API server started with PID %d", serverInfo.ApiServerPID)
	defer teInfo.ProcessExecutor.Dispose()
	defer shutdownAdvancedTestEnvironment(t, ctx, cancel, serverInfo)

	serverServiceName := containertest.UniqueName(t, "parrot-server-service")
	serverSvc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serverServiceName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              commonapi.PortProtocolTCP,
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

	serverExecutableName := containertest.UniqueName(t, "parrot-server")
	serverExe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serverExecutableName,
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

	clientServiceName := containertest.UniqueName(t, "parrot-client-service")
	clientSvc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clientServiceName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              commonapi.PortProtocolTCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}

	t.Logf("Creating Service '%s'", clientSvc.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, clientSvc)
	require.NoError(t, err, "Could not create the parrot client Service")

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      containertest.UniqueName(t, "parrot-network"),
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	proxyAlias := containertest.UniqueName(t, "parrot")
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              containertest.UniqueName(t, "parrot-tunnel"),
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

	parrotImageName := "localhost/" + containertest.UniqueName(t, "dcp-parrot-test-utility") + ":latest"
	require.NoError(t, resourceTracker.TrackImage(parrotImageName))
	parrotImage, parrotContainerPath, parrotImageErr := ensureParrotContainerImage(
		ctx,
		parrotContainerBinaryPath,
		parrotImageName,
		resourceTracker.MapLabels(),
		serverInfo.ContainerOrchestrator,
	)
	require.NoError(t, parrotImageErr, "Could not ensure parrot container image")

	clientContainerName := containertest.UniqueName(t, "parrot-client")
	clientCtr := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clientContainerName,
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
		},
	}
	if runtime.Name != string(container_flags.WslcRuntime) {
		// Optional ping/debug access is not exposed by the WSLC CLI.
		clientCtr.Spec.RunArgs = []string{"--cap-add=NET_RAW"}
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

	t.Log("Verifying the runtime published client proxy ports on the loopback interface only...")
	runningTunnelProxy := apiv1.ContainerNetworkTunnelProxy{}
	require.NoError(t, serverInfo.Client.Get(ctx, tunnelProxy.NamespacedName(), &runningTunnelProxy))
	physicalContainers := apiv2.PhysicalContainerList{}
	require.NoError(t, serverInfo.Client.List(ctx, &physicalContainers, ctrl_client.InNamespace(controllers.V1PhysicalResourcesNamespaceName)))
	clientProxyIndex := std_slices.IndexFunc(physicalContainers.Items, func(physicalContainer apiv2.PhysicalContainer) bool {
		return physicalContainer.Status.ContainerID != "" && physicalContainer.Status.ContainerID == runningTunnelProxy.Status.ClientProxyContainerID
	})
	require.NotEqual(t, -1, clientProxyIndex, "Client proxy PhysicalContainer should exist")
	clientProxyPortMappings := physicalContainers.Items[clientProxyIndex].Status.PortMappings
	for _, containerPort := range []int32{dcptun.DefaultContainerProxyControlPort, dcptun.DefaultContainerProxyDataPort} {
		hostBindings := slices.Select(clientProxyPortMappings, func(mapping apiv2.PhysicalContainerPortMapping) bool {
			return mapping.ContainerPort == containerPort && mapping.HostPort > 0
		})
		require.NotEmpty(t, hostBindings, "Client proxy port %d should be published", containerPort)
		for _, hostBinding := range hostBindings {
			hostIP := net.ParseIP(hostBinding.HostIP)
			require.True(t, hostIP != nil && hostIP.IsLoopback(),
				"Client proxy port %d should be published on a loopback address, not %q", containerPort, hostBinding.HostIP)
		}
	}
	if runtime.Name != string(container_flags.WslcRuntime) {
		requireTunnelProxyRuntimeCredentialIsolation(t, ctx, runtime, resourceTracker,
			physicalContainers.Items[clientProxyIndex], runningTunnelProxy.Status.ClientProxyContainerImage)
	}
}

func requireTunnelProxyRuntimeCredentialIsolation(
	t *testing.T,
	ctx context.Context,
	runtime containertest.Runtime,
	tracker *containertest.ResourceTracker,
	physicalContainer apiv2.PhysicalContainer,
	image string,
) {
	t.Helper()
	fileSystem := physicalContainer.Spec.Container.CreateFiles[0]
	securityPath := fileSystem.Destination + "/" + fileSystem.Entries[0].Name
	permissionCommand := exec.CommandContext(ctx, runtime.Name,
		"exec", physicalContainer.Status.ContainerID, "/bin/sh", "-c",
		`test "$(stat -c '%a:%u' "$1")" = "600:$(id -u)" && test -r "$1"`,
		"credential-permissions", securityPath,
	)
	permissionOutput, permissionErr := permissionCommand.CombinedOutput()
	require.NoError(t, permissionErr, "Runtime credential owner/mode check failed: %s", permissionOutput)
	unrelatedUserCommand := exec.CommandContext(ctx, runtime.Name,
		"exec", "--user", "65534", physicalContainer.Status.ContainerID, "/bin/sh", "-c",
		`test ! -r "$1"`, "credential-isolation", securityPath,
	)
	unrelatedUserOutput, unrelatedUserErr := unrelatedUserCommand.CombinedOutput()
	require.NoError(t, unrelatedUserErr, "An unrelated container user must not read credentials: %s", unrelatedUserOutput)

	probeName := containertest.UniqueName(t, "tunnel-image-credential-probe")
	require.NoError(t, tracker.TrackContainer(probeName))
	probeArgs := []string{
		"run", "--rm", "--pull=never", "--network=none", "--name", probeName, "--entrypoint", "/bin/sh",
	}
	for label, value := range tracker.MapLabels() {
		probeArgs = append(probeArgs, "--label", label+"="+value)
	}
	probeArgs = append(probeArgs, image, "-c", `test ! -e "$1"`, "image-credential-isolation", securityPath)
	imageProbeCommand := exec.CommandContext(ctx, runtime.Name, probeArgs...)
	imageProbeOutput, imageProbeErr := imageProbeCommand.CombinedOutput()
	require.NoError(t, imageProbeErr, "A fresh shared-image container must not contain credentials: %s", imageProbeOutput)
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

	shutdownTestEnvironment(t, serverInfo, cancel)
}

// The tunnel controller will try to create a server-side proxy
// and read the network configuration (control port in particular) off of it,
// so we need to simulate that.
func simulateServerProxy(
	t *testing.T,
	serverControlPort int32,
	tpe *internal_testutil.TestProcessExecutor,
) {
	simulateServerProxyWithLifecycleCallbacks(t, serverControlPort, tpe, nil, nil)
}

func simulateServerProxyWithLifecycleCallbacks(
	t *testing.T,
	serverControlPort int32,
	tpe *internal_testutil.TestProcessExecutor,
	onStartup func(),
	stopError func(*internal_testutil.ProcessExecution) error,
) {
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr, "Could not get DCP executable path")

	tpe.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{dcpPath, "tunnel-server"},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			if onStartup != nil {
				onStartup()
			}
			tc := dcptun.TunnelProxyConfig{
				ServerControlPort: serverControlPort,
			}
			tcBytes, tcErr := json.Marshal(tc)
			if tcErr != nil {
				t.Errorf("Could not marshal TunnelProxyConfig to JSON: %v", tcErr)
				return 1
			}
			if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(tcBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyConfig to stdout: %v", writeErr)
				return 1
			}
			readinessBytes, readinessErr := json.Marshal(dcptun.TunnelProxyReadiness{ControlReady: true})
			if readinessErr != nil {
				t.Errorf("Could not marshal TunnelProxyReadiness to JSON: %v", readinessErr)
				return 1
			}
			if _, readinessWriteErr := pe.Cmd.Stdout.Write(osutil.WithNewline(readinessBytes)); readinessWriteErr != nil && !errors.Is(readinessWriteErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyReadiness to stdout: %v", readinessWriteErr)
				return 1
			}

			select {
			case <-pe.Signal:
			case <-t.Context().Done():
			}
			return 0
		},
		StopError: stopError,
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
				Protocol:              commonapi.PortProtocolTCP,
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
				Protocol:              commonapi.PortProtocolTCP,
				AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
			},
		}
		t.Logf("Creating client Service '%s' for tunnel %d", clientSvcName, i)
		createClientSvcErr := apiClient.Create(ctx, &clientSvc)
		require.NoError(t, createClientSvcErr, "Could not create client Service '%s'", clientSvcName)

		// Enable tunnel in test control client so controller can prepare it
		tunnelSpec := &dcptunproto.TunnelSpec{
			ServerAddress:   stdproto.String(networking.IPv4LocalhostDefaultAddress),
			ServerPort:      stdproto.Int32(serverServicePort),
			ClientProxyPort: stdproto.Int32(serverServicePort),
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
	imageName string,
	labels map[string]string,
	ior containers.ImageOrchestrator,
) (string, string, error) {
	const parrotContainerPath = "/usr/local/bin/parrot"

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

	// The parrot container build rule pins CGO off so this binary can run without a loader.
	dockerfileContent := fmt.Sprintf(
		"FROM scratch\n\nCOPY --chmod=0755 parrot %[1]s\nENTRYPOINT [\"%[1]s\"]\n",
		parrotContainerPath,
	)

	// Write Dockerfile
	dockerfilePath := filepath.Join(tempDir, "Dockerfile")
	if writeErr := usvc_io.WriteFile(dockerfilePath, []byte(dockerfileContent), osutil.PermissionOnlyOwnerReadWrite); writeErr != nil {
		return "", "", fmt.Errorf("failed to write Dockerfile: %w", writeErr)
	}

	// Build the image
	buildOptions := containers.BuildImageOptions{
		ContainerBuildContext: &containers.ContainerBuildContext{
			Context:    tempDir,
			Dockerfile: dockerfilePath,
			Tags:       []string{imageName},
			Labels:     labels,
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
	sourceFile, sourceErr := usvc_io.OpenFileReadOnly(src)
	if sourceErr != nil {
		return sourceErr
	}
	defer func() { _ = sourceFile.Close() }()

	destFile, destErr := usvc_io.CreateNewFile(dst, osutil.PermissionOnlyOwnerReadWriteExecute)
	if destErr != nil {
		return destErr
	}

	if _, copyErr := io.Copy(destFile, sourceFile); copyErr != nil {
		_ = destFile.Close()
		return copyErr
	}

	return destFile.Close()
}

type tunnelProxyRecoveryMessageClient struct {
	ctrl_client.Client
	messages chan<- string
}

func (c *tunnelProxyRecoveryMessageClient) Status() ctrl_client.SubResourceWriter {
	return &tunnelProxyRecoveryMessageWriter{
		SubResourceWriter: c.Client.Status(),
		messages:          c.messages,
	}
}

type tunnelProxyRecoveryMessageWriter struct {
	ctrl_client.SubResourceWriter
	messages chan<- string
}

func (w *tunnelProxyRecoveryMessageWriter) Patch(
	ctx context.Context,
	object ctrl_client.Object,
	patch ctrl_client.Patch,
	options ...ctrl_client.SubResourcePatchOption,
) error {
	patchErr := w.SubResourceWriter.Patch(ctx, object, patch, options...)
	if patchErr != nil {
		return patchErr
	}
	if proxy, isProxy := object.(*apiv1.ContainerNetworkTunnelProxy); isProxy && proxy.Status.Message != "" {
		switch proxy.Status.State {
		case apiv1.ContainerNetworkTunnelProxyStateRecoveryPending,
			apiv1.ContainerNetworkTunnelProxyStateRecoveryCleanup,
			apiv1.ContainerNetworkTunnelProxyStateRecoveryWait:
			select {
			case w.messages <- proxy.Status.Message:
			default:
			}
		}
	}
	return nil
}

type testDelegatingTunnelControlServer struct {
	dcptunproto.UnimplementedTunnelControlServer
	client *ctrl_testutil.TestTunnelControlClient
}

func (s *testDelegatingTunnelControlServer) PrepareTunnel(
	ctx context.Context,
	req *dcptunproto.TunnelReq,
) (*dcptunproto.TunnelSpec, error) {
	return s.client.PrepareTunnel(ctx, req)
}

func (s *testDelegatingTunnelControlServer) UpdateTunnel(
	ctx context.Context,
	update *dcptunproto.TunnelUpdate,
) (*emptypb.Empty, error) {
	return s.client.UpdateTunnel(ctx, update)
}

func (s *testDelegatingTunnelControlServer) DeleteTunnel(
	ctx context.Context,
	ref *dcptunproto.TunnelRef,
) (*emptypb.Empty, error) {
	return s.client.DeleteTunnel(ctx, ref)
}

func (s *testDelegatingTunnelControlServer) Shutdown(
	_ context.Context,
	_ *emptypb.Empty,
) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// Verifies that ContainerNetworkTunnelProxyReconciler authenticates its real gRPC control client
// against an authenticated dcptun control server using the per-generation TLS CA and bearer token,
// while a client presenting the server proxy's outgoing (wrong-role) token is rejected.
func TestTunnelProxyControllerAuthenticatesWithRealControlServer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "real-authenticated-control-client"

	serverInfo, teInfo, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				config.MakeTunnelControlClient = dcptunproto.NewTunnelControlClient
			},
		},
	)
	require.NoError(t, startErr)

	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	capturedSecurity := concurrency.NewValuePromise[dcptun.TunnelControlSecurityConfig]()
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			securityBytes, readErr := io.ReadAll(pe.Cmd.Stdin)
			if readErr != nil {
				t.Errorf("Could not read server control security stdin: %v", readErr)
				return 1
			}
			var serverSecurity dcptun.TunnelControlSecurityConfig
			if unmarshalErr := json.Unmarshal(securityBytes, &serverSecurity); unmarshalErr != nil {
				t.Errorf("Could not decode server control security stdin: %v", unmarshalErr)
				return 1
			}
			if validateErr := errors.Join(serverSecurity.ValidateServer(), serverSecurity.ValidateClient()); validateErr != nil {
				t.Errorf("Invalid server control security config: %v", validateErr)
				return 1
			}
			capturedSecurity.Set(serverSecurity)

			listener, listenErr := net.Listen("tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, 0))
			if listenErr != nil {
				t.Errorf("Could not bind server control listener: %v", listenErr)
				return 1
			}
			defer func() { _ = listener.Close() }()

			grpcServer, serverErr := dcptun.NewControlServer(serverSecurity, &testDelegatingTunnelControlServer{
				client: teInfo.TestTunnelControlClient,
			})
			if serverErr != nil {
				t.Errorf("Could not create authenticated gRPC control server: %v", serverErr)
				return 1
			}
			defer grpcServer.Stop()

			go func() {
				if serveErr := grpcServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
					t.Errorf("Authenticated control server failed: %v", serveErr)
				}
			}()

			boundPort := int32(listener.Addr().(*net.TCPAddr).Port)
			cfgBytes, cfgErr := json.Marshal(dcptun.TunnelProxyConfig{
				ServerControlAddress: networking.IPv4LocalhostDefaultAddress,
				ServerControlPort:    boundPort,
			})
			if cfgErr != nil {
				t.Errorf("Could not encode TunnelProxyConfig: %v", cfgErr)
				return 1
			}
			if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(cfgBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyConfig: %v", writeErr)
				return 1
			}
			readyBytes, readyErr := json.Marshal(dcptun.TunnelProxyReadiness{ControlReady: true})
			if readyErr != nil {
				t.Errorf("Could not encode TunnelProxyReadiness: %v", readyErr)
				return 1
			}
			if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(readyBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyReadiness: %v", writeErr)
				return 1
			}

			select {
			case <-pe.Signal:
			case <-ctx.Done():
			}
			return 0
		},
	})

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	tunnels := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName, 36100)
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              tunnels[0].tunnelName,
					ServerServiceName: tunnels[0].serverServiceName,
					ClientServiceName: tunnels[0].clientServiceName,
				},
			},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))
	running := waitAllTunnelsInState(t, ctx, serverInfo.Client, proxy.NamespacedName(), 1, apiv1.TunnelStateReady)
	validateTunnel(t, ctx, running.Status.TunnelStatuses[0], tunnels[0], serverInfo.Client, teInfo.TestTunnelControlClient)

	// Verify the live server rejects a client using the server->client OutgoingToken against the controller->server endpoint.
	serverSec := capturedSecurity.Get()
	wrongHopSecurity := dcptun.TunnelControlSecurityConfig{
		// Trusting the server's leaf certificate directly lets the TLS handshake succeed,
		// so the request reaches bearer-token verification.
		PeerCACertBase64: serverSec.ServerCertBase64,
		OutgoingToken:    serverSec.OutgoingToken,
	}
	wrongDialOpts, wrongOptsErr := wrongHopSecurity.ClientDialOptions()
	require.NoError(t, wrongOptsErr)
	wrongConn, wrongConnErr := grpc.NewClient(
		networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, running.Status.ServerProxyControlPort),
		wrongDialOpts...,
	)
	require.NoError(t, wrongConnErr)
	defer func() { _ = wrongConn.Close() }()
	_, wrongAuthErr := dcptunproto.NewTunnelControlClient(wrongConn).DeleteTunnel(ctx, &dcptunproto.TunnelRef{TunnelId: stdproto.Uint32(1)})
	require.Equal(t, codes.Unauthenticated, status.Code(wrongAuthErr))

	// Remove the tunnel from Spec.Tunnels and verify real DeleteTunnel completes over the authenticated control channel.
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = nil
		return serverInfo.Client.Update(updateCtx, current)
	}))
	validateTunnelDeleted(t, ctx, serverInfo.Client, tunnels[0], teInfo.TestTunnelControlClient)
}

// Verifies that deleting a ContainerNetworkTunnelProxy while its server proxy is blocked waiting
// for initial control-stream readiness cancels the startup wait immediately instead of blocking for ControlStartupTimeout.
func TestTunnelProxyDeletionInterruptsUnreadyServerStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "delete-interrupts-unready-startup"

	serverInfo, teInfo, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				config.ControlStartupTimeout = 10 * time.Minute
			},
		},
	)
	require.NoError(t, startErr)

	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	configWritten := concurrency.NewValuePromise[bool]()
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			cfgBytes, cfgErr := json.Marshal(dcptun.TunnelProxyConfig{ServerControlPort: 16801})
			if cfgErr != nil {
				t.Errorf("Could not encode TunnelProxyConfig: %v", cfgErr)
				return 1
			}
			if _, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(cfgBytes)); writeErr != nil && !errors.Is(writeErr, os.ErrClosed) {
				t.Errorf("Could not write TunnelProxyConfig: %v", writeErr)
				return 1
			}
			configWritten.Set(true)
			select {
			case <-pe.Signal:
			case <-ctx.Done():
			}
			return 0
		},
	})

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))

	select {
	case <-configWritten.Wait():
	case <-ctx.Done():
		t.Fatal("Server proxy did not reach readiness wait")
	}

	deleteStarted := time.Now()
	require.NoError(t, serverInfo.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, proxy)
	require.Less(t, time.Since(deleteStarted), 15*time.Second, "Deletion must interrupt ControlStartupTimeout promptly")

	remainingContainers, listErr := serverInfo.ContainerOrchestrator.ListContainers(ctx, containers.ListContainersOptions{})
	require.NoError(t, listErr)
	require.Empty(t, remainingContainers)
}

// Verifies that removing a tunnel from Spec.Tunnels after a lost PrepareTunnel response (while TunnelID == 0)
// deletes the prepared tunnel from the proxy pair by name instead of leaving it bound.
func TestTunnelProxyDeletingTunnelAfterLostPrepareResponseCleansUpProxy(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "delete-tunnel-after-lost-prepare"

	lostResponse := &lostTunnelPreparationResponse{
		firstApplied: make(chan *dcptunproto.TunnelSpec, 1),
		releaseFirst: make(chan struct{}),
		retryRequest: make(chan *dcptunproto.TunnelReq, 1),
	}
	lostResponse.loseNext.Store(true)

	serverInfo, teInfo, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		ServiceController|NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
				makeClient := config.MakeTunnelControlClient
				config.MakeTunnelControlClient = func(conn grpc.ClientConnInterface) dcptunproto.TunnelControlClient {
					lostResponse.TunnelControlClient = makeClient(conn)
					return lostResponse
				}
			},
		},
	)
	require.NoError(t, startErr)
	simulateServerProxy(t, 16802, teInfo.TestProcessExecutor)

	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	tunnels := prepareTunnelServices(t, ctx, serverInfo.Client, teInfo.TestTunnelControlClient, 1, testName, 36200)
	proxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: testName},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:              tunnels[0].tunnelName,
					ServerServiceName: tunnels[0].serverServiceName,
					ClientServiceName: tunnels[0].clientServiceName,
				},
			},
		},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, proxy))

	select {
	case <-lostResponse.firstApplied:
	case <-ctx.Done():
		t.Fatal("Initial PrepareTunnel did not reach proxy")
	}
	require.True(t, teInfo.TestTunnelControlClient.HasPreparedTunnel(tunnels[0].tunnelName))

	// Remove the tunnel from Spec.Tunnels while the lost-response PrepareTunnel call is still blocked,
	// guaranteeing the very next reconciliation enters Deleting with TunnelID == 0 and no retry race.
	require.NoError(t, retryOnConflictEx(ctx, serverInfo.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv1.ContainerNetworkTunnelProxy) error {
		current.Spec.Tunnels = nil
		return serverInfo.Client.Update(updateCtx, current)
	}))
	close(lostResponse.releaseFirst)

	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning && len(current.Status.TunnelStatuses) == 0, nil
	})
	require.False(t, teInfo.TestTunnelControlClient.HasPreparedTunnel(tunnels[0].tunnelName), "Removed tunnel must be deleted from proxy pair by name")
}

// Verifies that server-proxy config-read timeouts while the process is still running retry via
// neverReadyGenerations, whereas invalid ServerControlPort values fail immediately.
func TestTunnelProxyConfigReadTimeoutRecoversAndInvalidPortFails(t *testing.T) {
	t.Parallel()

	t.Run("config-read-timeout-recovers", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		dcppaths.EnableTestPathProbing()
		const testName = "config-read-timeout-recovers"

		serverInfo, teInfo, startErr := StartTestEnvironmentWithOptions(
			t,
			ctx,
			NetworkController|ContainerNetworkTunnelProxyController,
			t.Name(),
			t.TempDir(),
			TestEnvironmentOptions{
				ConfigureTunnelProxy: func(config *controllers.ContainerNetworkTunnelProxyReconcilerConfig) {
					config.ServerConfigReadTimeout = 500 * time.Millisecond
					config.PairRestartBackoffFactory = func() backoff.BackOff {
						return backoff.NewConstantBackOff(time.Millisecond)
					}
				},
			},
		)
		require.NoError(t, startErr)

		dcpPath, pathErr := dcppaths.GetDcpExePath()
		require.NoError(t, pathErr)
		var starts atomic.Int32
		teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
			Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
			RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
				if starts.Add(1) == 1 {
					// Stall without writing configuration until stopped after ServerConfigReadTimeout.
					select {
					case <-pe.Signal:
					case <-ctx.Done():
					}
					return 0
				}
				cfgBytes, _ := json.Marshal(dcptun.TunnelProxyConfig{ServerControlPort: 16803})
				readyBytes, _ := json.Marshal(dcptun.TunnelProxyReadiness{ControlReady: true})
				_, _ = pe.Cmd.Stdout.Write(osutil.WithNewline(cfgBytes))
				_, _ = pe.Cmd.Stdout.Write(osutil.WithNewline(readyBytes))
				select {
				case <-pe.Signal:
				case <-ctx.Done():
				}
				return 0
			},
		})

		network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
		require.NoError(t, serverInfo.Client.Create(ctx, network))
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{Name: testName},
			Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
		}
		require.NoError(t, serverInfo.Client.Create(ctx, proxy))

		running := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
			if current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed {
				return false, fmt.Errorf("config-read timeout failed permanently instead of recovering: %s", current.Status.Message)
			}
			return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
		})
		require.GreaterOrEqual(t, starts.Load(), int32(2))
		require.Equal(t, int32(16803), running.Status.ServerProxyControlPort)
	})

	t.Run("invalid-control-port-is-terminal", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		dcppaths.EnableTestPathProbing()
		const testName = "invalid-control-port-terminal"

		serverInfo, teInfo, startErr := StartTestEnvironment(
			t,
			ctx,
			NetworkController|ContainerNetworkTunnelProxyController,
			t.Name(),
			t.TempDir(),
		)
		require.NoError(t, startErr)

		dcpPath, pathErr := dcppaths.GetDcpExePath()
		require.NoError(t, pathErr)
		var starts atomic.Int32
		teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
			Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
			RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
				starts.Add(1)
				_, _ = io.WriteString(pe.Cmd.Stdout, "{}\n")
				select {
				case <-pe.Signal:
				case <-ctx.Done():
				}
				return 0
			},
		})

		network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: testName + "-network"}}
		require.NoError(t, serverInfo.Client.Create(ctx, network))
		proxy := &apiv1.ContainerNetworkTunnelProxy{
			ObjectMeta: metav1.ObjectMeta{Name: testName},
			Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
		}
		require.NoError(t, serverInfo.Client.Create(ctx, proxy))

		failed := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, proxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
			return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed && current.Status.ServerProxyProcessID == nil, nil
		})
		require.Contains(t, failed.Status.Message, "invalid server control port 0")
		require.Equal(t, int32(1), starts.Load())
	})
}

type stderrCaptureLogSink struct {
	inner       logr.LogSink
	stderrFound *concurrency.ValuePromise[string]
}

func (s *stderrCaptureLogSink) Init(info logr.RuntimeInfo) {
	if s.inner != nil {
		s.inner.Init(info)
	}
}

func (s *stderrCaptureLogSink) Enabled(level int) bool {
	if s.inner != nil {
		return s.inner.Enabled(level)
	}
	return true
}

func (s *stderrCaptureLogSink) Info(level int, msg string, keysAndValues ...any) {
	if s.inner != nil {
		s.inner.Info(level, msg, keysAndValues...)
	}
}

func (s *stderrCaptureLogSink) Error(err error, msg string, keysAndValues ...any) {
	for idx := 0; idx+1 < len(keysAndValues); idx += 2 {
		if key, ok := keysAndValues[idx].(string); ok && key == "ServerStderr" {
			if val, valOK := keysAndValues[idx+1].(string); valOK && val != "" {
				s.stderrFound.Set(val)
			}
		}
	}
	if s.inner != nil {
		s.inner.Error(err, msg, keysAndValues...)
	}
}

func (s *stderrCaptureLogSink) WithValues(keysAndValues ...any) logr.LogSink {
	var nextInner logr.LogSink
	if s.inner != nil {
		nextInner = s.inner.WithValues(keysAndValues...)
	}
	return &stderrCaptureLogSink{inner: nextInner, stderrFound: s.stderrFound}
}

func (s *stderrCaptureLogSink) WithName(name string) logr.LogSink {
	var nextInner logr.LogSink
	if s.inner != nil {
		nextInner = s.inner.WithName(name)
	}
	return &stderrCaptureLogSink{inner: nextInner, stderrFound: s.stderrFound}
}

// Verifies that server-proxy output files are removed when a running proxy is deleted,
// and that failed-start stderr diagnostics are logged before failed-start temp files are removed.
func TestTunnelProxyCleansUpServerOutputFilesAndLogsFailedStartStderr(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()

	loggedStderr := concurrency.NewValuePromise[string]()
	serverInfo, teInfo, startErr := StartTestEnvironmentWithOptions(
		t,
		ctx,
		NetworkController|ContainerNetworkTunnelProxyController,
		t.Name(),
		t.TempDir(),
		TestEnvironmentOptions{
			DecorateLogger: func(base logr.Logger) logr.Logger {
				return logr.New(&stderrCaptureLogSink{
					inner:       base.GetSink(),
					stderrFound: loggedStderr,
				})
			},
		},
	)
	require.NoError(t, startErr)

	// 1. Verify a running proxy's stdout/stderr files are deleted when the proxy is deleted.
	simulateServerProxy(t, 16804, teInfo.TestProcessExecutor)
	network := &apiv1.ContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "output-cleanup-network"}}
	require.NoError(t, serverInfo.Client.Create(ctx, network))
	runningProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "running-output-cleanup"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, runningProxy))
	running := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, runningProxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})
	runningStdout := running.Status.ServerProxyStdOutFile
	runningStderr := running.Status.ServerProxyStdErrFile
	require.FileExists(t, runningStdout)
	require.FileExists(t, runningStderr)

	require.NoError(t, serverInfo.Client.Delete(ctx, running))
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, running)
	require.NoFileExists(t, runningStdout, "Deleted proxy stdout file must be removed")
	require.NoFileExists(t, runningStderr, "Deleted proxy stderr file must be removed")

	// 2. Verify a failed startup where the server is stopped by cleanupFailedStart logs bounded
	// stderr diagnostics before deleting its temp files (without racing onServerProcessExit).
	dcpPath, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	const expectedDiagnostic = "TLS control bootstrap diagnostic failure"
	var failedStderrPath atomic.Value
	teInfo.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{dcpPath, "tunnel-server"}},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			if stderrFile, ok := pe.Cmd.Stderr.(*os.File); ok {
				failedStderrPath.Store(stderrFile.Name())
			}
			_, _ = io.WriteString(pe.Cmd.Stderr, expectedDiagnostic+"\n")
			_, _ = io.WriteString(pe.Cmd.Stdout, "invalid configuration\n")
			select {
			case <-pe.Signal:
			case <-ctx.Done():
			}
			return 0
		},
	})

	failingProxy := &apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-stderr-logging"},
		Spec:       apiv1.ContainerNetworkTunnelProxySpec{ContainerNetworkName: network.Name},
	}
	require.NoError(t, serverInfo.Client.Create(ctx, failingProxy))
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, failingProxy.NamespacedName(), func(current *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return current.Status.State == apiv1.ContainerNetworkTunnelProxyStateFailed, nil
	})

	select {
	case <-loggedStderr.Wait():
		require.Contains(t, loggedStderr.Get(), expectedDiagnostic)
	case <-ctx.Done():
		t.Fatal("Failed server startup did not log ServerStderr diagnostic output")
	}
	recordedPath, hasPath := failedStderrPath.Load().(string)
	require.True(t, hasPath)
	require.NoFileExists(t, recordedPath)
}
