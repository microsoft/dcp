/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	stdproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// logicalTunnelServerBehavior adjusts the simulated tunnel-server installed by installLogicalTunnelServer.
type logicalTunnelServerBehavior struct {
	// withholdReadiness omits the control readiness line, leaving startup blocked until the process is stopped.
	withholdReadiness bool
	// stderr is written to the server's stderr before it reports its configuration.
	stderr string
	// stopError overrides process stop handling, as AutoExecution.StopError does.
	stopError func(*internal_testutil.ProcessExecution) error
}

// installLogicalTunnelServer simulates tunnel-server: it reads credentials from stdin, serves authenticated control
// on a loopback port, and reports that port and control readiness on stdout.
func installLogicalTunnelServer(t *testing.T, ctx context.Context, environment *TestEnvironmentInfo, behavior logicalTunnelServerBehavior) {
	t.Helper()
	executable, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{executable, "tunnel-server"}},
		StopError: behavior.stopError,
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			var security dcptun.TunnelControlSecurityConfig
			if decodeErr := json.NewDecoder(execution.Cmd.Stdin).Decode(&security); decodeErr != nil {
				t.Error(decodeErr)
				return 1
			}
			server, serverErr := dcptun.NewControlServer(security, &dcptunproto.UnimplementedTunnelControlServer{})
			if serverErr != nil {
				t.Error(serverErr)
				return 1
			}
			listener, listenErr := (&net.ListenConfig{}).Listen(ctx, "tcp", networking.AddressAndPort(networking.IPv4LocalhostDefaultAddress, 0))
			if listenErr != nil {
				t.Error(listenErr)
				return 1
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			if behavior.stderr != "" {
				if _, writeErr := execution.Cmd.Stderr.Write(osutil.WithNewline([]byte(behavior.stderr))); writeErr != nil {
					t.Error(writeErr)
					return 1
				}
			}
			port := int32(listener.Addr().(*net.TCPAddr).Port)
			lines := []any{dcptun.TunnelProxyConfig{ServerControlPort: port}}
			if !behavior.withholdReadiness {
				lines = append(lines, dcptun.TunnelProxyReadiness{ControlReady: true})
			}
			for _, line := range lines {
				encoded, encodeErr := json.Marshal(line)
				if encodeErr != nil {
					t.Error(encodeErr)
					return 1
				}
				if _, writeErr := execution.Cmd.Stdout.Write(osutil.WithNewline(encoded)); writeErr != nil {
					t.Error(writeErr)
					return 1
				}
			}
			select {
			case _, open := <-execution.Signal:
				if !open {
					t.Error("tunnel server signal channel closed unexpectedly")
				}
			case <-ctx.Done():
			}
			server.Stop()
			if serveErr := <-done; serveErr != nil {
				t.Error(serveErr)
			}
			return 0
		},
	})
}

// waitLogicalTunnelServerStopped waits until the host tunnel server with the given PID is no longer running.
func waitLogicalTunnelServerStopped(t *testing.T, ctx context.Context, environment *TestEnvironmentInfo, pid *int64) {
	t.Helper()
	require.NotNil(t, pid)
	serverPID, pidErr := process.Int64_ToPidT(*pid)
	require.NoError(t, pidErr)
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
		_, findErr := environment.TestProcessExecutor.FindProcessHandle(serverPID)
		return errors.Is(findErr, process.ErrorProcessNotFound), nil
	}), "the host tunnel server must be stopped")
}

func startLogicalTunnelApplication(
	t *testing.T, ctx context.Context, options TestEnvironmentOptions,
) (*ctrl_testutil.ApiServerInfo, *TestEnvironmentInfo, *apiv2.LogicalContainerTunnelProxy) {
	t.Helper()
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerTunnelProxyController|LogicalServiceController, t.Name(), t.TempDir(), options)
	require.NoError(t, startupErr)
	installLogicalTunnelServer(t, ctx, environment, logicalTunnelServerBehavior{})
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tunnel-app"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
		return current.Status.Phase == apiv2.NamespacePhaseActive, nil
	})
	network := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}}}
	require.NoError(t, server.Client.Create(ctx, network))
	host := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: namespace.Name}}
	require.NoError(t, server.Client.Create(ctx, host))
	allocated := waitObjectAssumesStateEx(t, ctx, server.Client, host.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return len(current.Status.EffectiveAddresses) > 0, nil
	})
	address, found, selectionErr := networking.SelectServiceAddress(allocated.Status.EffectiveAddresses,
		networking.IpVersionPreferenceNone, networking.GetIpVersionPreference())
	require.NoError(t, selectionErr)
	require.True(t, found)
	environment.EnableTunnel(&dcptunproto.TunnelSpec{ServerAddress: stdproto.String(address.Address), ServerPort: stdproto.Int32(address.Port),
		ClientProxyPort: stdproto.Int32(address.Port)})
	containerService := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "client", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
	require.NoError(t, server.Client.Create(ctx, containerService))
	proxy := &apiv2.LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerTunnelProxySpec{NetworkRef: "network", BaseImage: "example.com/tunnel-base:custom",
			Aliases: []string{"host-tunnel"}, Tunnels: []apiv2.LogicalContainerTunnel{{Name: "http", ServerServiceRef: "host", ClientServiceRef: "client"}}}}
	return server, environment, proxy
}

func tunnelProxyReadyReason(proxy *apiv2.LogicalContainerTunnelProxy) apiv2.ConditionReason {
	condition := apimeta.FindStatusCondition(proxy.Status.Conditions, string(apiv2.ConditionReady))
	if condition == nil {
		return ""
	}
	return apiv2.ConditionReason(condition.Reason)
}

// Verifies that LogicalContainerTunnelProxy owns namespaced physical backings, honors baseImage, launches the host server with
// stdin credentials, publishes Proxyless service endpoints, supports tunnel removal and re-addition without replacing the proxy pair,
// and stops the server and removes its output on deletion.
func TestV2LogicalContainerTunnelProxyLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
	})
	require.NotEmpty(t, running.Status.ContainerID)
	require.NotNil(t, running.Status.PID)
	container, image := &apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalContainerRef}, container))
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID)}, image))
	for _, child := range []ctrl_client.Object{container, image} {
		require.True(t, metav1.IsControlledBy(child, proxy))
		require.Contains(t, child.GetFinalizers(), controllers.LogicalBackingFinalizer)
	}
	require.Equal(t, []string{proxy.Spec.BaseImage}, image.Spec.Image.Build.BaseImages)
	require.True(t, image.Spec.RemoveBuildContextArchive)
	require.Equal(t, environment.SessionFolder, filepath.Dir(image.Spec.Image.Build.ContextArchive.Source))
	require.Equal(t, proxy.Spec.Aliases, container.Spec.Container.Networks[0].Aliases)
	serverPID, pidErr := process.Int64_ToPidT(*running.Status.PID)
	require.NoError(t, pidErr)
	hostServer, serverFound := environment.TestProcessExecutor.FindByPid(serverPID)
	require.True(t, serverFound)
	require.Contains(t, hostServer.Cmd.Args, "--control-security-stdin")
	require.NotContains(t, hostServer.Cmd.Args, "--control-security-file")
	serverOutput := []string{hostServer.Cmd.Stdout.(*os.File).Name(), hostServer.Cmd.Stderr.(*os.File).Name()}
	require.Len(t, container.Spec.Container.CreateFiles, 1)
	require.Len(t, container.Spec.Container.CreateFiles[0].Entries, 1)
	clientSecurityPath := container.Spec.Container.CreateFiles[0].Entries[0].Source
	clientSecurityJSON, clientReadErr := os.ReadFile(clientSecurityPath)
	require.NoError(t, clientReadErr)
	var clientSecurity dcptun.TunnelControlSecurityConfig
	require.NoError(t, json.Unmarshal(clientSecurityJSON, &clientSecurity))
	require.NoError(t, clientSecurity.ValidateServer())
	require.Empty(t, clientSecurity.PeerCACertBase64)
	require.Empty(t, clientSecurity.OutgoingToken)
	require.NotContains(t, strings.Join(container.Spec.Container.Command, " "), clientSecurity.ServerKeyBase64)
	require.NotContains(t, strings.Join(hostServer.Cmd.Args, " "), clientSecurity.ServerKeyBase64)
	endpointName := types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID) + "-http"}
	endpoint := waitObjectAssumesStateEx(t, ctx, server.Client, endpointName, func(current *apiv2.ServiceEndpoint) (bool, error) {
		return len(current.Spec.Addresses) > 0, nil
	})
	require.True(t, metav1.IsControlledBy(endpoint, proxy))
	clientService := waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: proxy.Namespace, Name: "client"}, func(current *apiv2.LogicalService) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalServicePhaseRunning && ready != nil && ready.Status == metav1.ConditionTrue, nil
	})
	require.Equal(t, endpoint.Spec.Addresses, clientService.Status.EffectiveAddresses)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerTunnelProxy) error {
		current.Spec.Tunnels = nil
		return server.Client.Update(updateCtx, current)
	}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, endpoint)
	removed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return len(current.Status.TunnelStatuses) == 0 && current.Status.TunnelConfigurationVersion > running.Status.TunnelConfigurationVersion, nil
	})
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerTunnelProxy) error {
		current.Spec.Tunnels = slices.Clone(proxy.Spec.Tunnels)
		return server.Client.Update(updateCtx, current)
	}))
	readded := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady &&
			current.Status.TunnelConfigurationVersion > removed.Status.TunnelConfigurationVersion, nil
	})
	require.Equal(t, running.Status.ContainerID, readded.Status.ContainerID)
	require.Equal(t, running.Status.PID, readded.Status.PID)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, container)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, image)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, endpoint)
	waitLogicalTunnelServerStopped(t, ctx, environment, running.Status.PID)
	for _, path := range serverOutput {
		require.NoFileExists(t, path)
	}
	require.NoFileExists(t, clientSecurityPath)
	require.NoDirExists(t, filepath.Dir(clientSecurityPath))
	require.NoError(t, server.Client.Get(ctx, clientService.NamespacedName(), &apiv2.LogicalService{}))
}

type tunnelRetirementFailureClient struct {
	dcptunproto.TunnelControlClient
	blocked atomic.Bool
	failed  atomic.Bool
}

func (boundary *tunnelRetirementFailureClient) DeleteTunnel(
	ctx context.Context, reference *dcptunproto.TunnelRef, options ...grpc.CallOption,
) (*emptypb.Empty, error) {
	if boundary.blocked.Load() {
		boundary.failed.Store(true)
		return nil, errors.New("injected tunnel retirement failure")
	}
	return boundary.TunnelControlClient.DeleteTunnel(ctx, reference, options...)
}

// Verifies that LogicalContainerTunnelProxy publishes a versioned availability change when retirement fails,
// then retires the tunnel without waiting for the referenced service to return or preparing a replacement.
func TestV2LogicalContainerTunnelProxyRetriesUnavailableTunnelRetirement(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var boundary *tunnelRetirementFailureClient
	server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
		DecorateTunnelControlClient: func(client dcptunproto.TunnelControlClient) dcptunproto.TunnelControlClient {
			boundary = &tunnelRetirementFailureClient{TunnelControlClient: client}
			boundary.blocked.Store(true)
			return boundary
		},
	})
	defer boundary.blocked.Store(false)
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
	})
	endpoint := waitObjectAssumesStateEx(t, ctx, server.Client,
		types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID) + "-http"},
		func(current *apiv2.ServiceEndpoint) (bool, error) { return len(current.Spec.Addresses) > 0, nil })
	host := &apiv2.LogicalService{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: "host"}, host))
	address, found, selectionErr := networking.SelectServiceAddress(host.Status.EffectiveAddresses,
		networking.IpVersionPreferenceNone, networking.GetIpVersionPreference())
	require.NoError(t, selectionErr)
	require.True(t, found)
	fingerprint := dcptunproto.TunnelRequestFingerprint{
		ServerAddress: address.Address, ServerPort: address.Port, ClientProxyPort: address.Port,
	}
	require.NoError(t, server.Client.Delete(ctx, host))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, host)
	unavailable := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return boundary.failed.Load() && len(current.Status.TunnelStatuses) == 1 &&
			current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateNotReady &&
			current.Status.TunnelStatuses[0].TunnelID == running.Status.TunnelStatuses[0].TunnelID, nil
	})
	require.Greater(t, unavailable.Status.TunnelConfigurationVersion, running.Status.TunnelConfigurationVersion)
	require.True(t, unavailable.Status.TunnelStatuses[0].Timestamp.After(running.Status.TunnelStatuses[0].Timestamp.Time))
	boundary.blocked.Store(false)
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateNotReady &&
			current.Status.TunnelStatuses[0].TunnelID == 0 &&
			environment.GetTunnelSpec(fingerprint).GetTunnelRef().GetTunnelId() == 0, nil
	})
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, endpoint)
	require.True(t, apierrors.IsNotFound(server.Client.Get(ctx, host.NamespacedName(), &apiv2.LogicalService{})))
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

type tunnelLostPreparationClient struct {
	dcptunproto.TunnelControlClient
	lock     sync.Mutex
	prepared map[string]int32
	applied  chan struct{}
	release  chan struct{}
	lost     atomic.Bool
}

// PrepareTunnel loses the first applied response and rejects changed requests for an existing name, as dcptun does.
func (boundary *tunnelLostPreparationClient) PrepareTunnel(
	ctx context.Context, request *dcptunproto.TunnelReq, options ...grpc.CallOption,
) (*dcptunproto.TunnelSpec, error) {
	if boundary.lost.Load() {
		select {
		case <-boundary.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	boundary.lock.Lock()
	port, exists := boundary.prepared[request.GetTunnelName()]
	if exists && port != request.GetServerPort() {
		boundary.lock.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "tunnel %q already exists with different configuration", request.GetTunnelName())
	}
	prepared, prepareErr := boundary.TunnelControlClient.PrepareTunnel(ctx, request, options...)
	if prepareErr == nil {
		boundary.prepared[request.GetTunnelName()] = request.GetServerPort()
	}
	boundary.lock.Unlock()
	if prepareErr != nil || !boundary.lost.CompareAndSwap(false, true) {
		return prepared, prepareErr
	}
	close(boundary.applied)
	select {
	case <-boundary.release:
	case <-ctx.Done():
	}
	return nil, errors.New("injected lost tunnel preparation response")
}

func (boundary *tunnelLostPreparationClient) DeleteTunnel(
	ctx context.Context, reference *dcptunproto.TunnelRef, options ...grpc.CallOption,
) (*emptypb.Empty, error) {
	deleted, deleteErr := boundary.TunnelControlClient.DeleteTunnel(ctx, reference, options...)
	if deleteErr == nil && reference.GetTunnelName() != "" {
		boundary.lock.Lock()
		delete(boundary.prepared, reference.GetTunnelName())
		boundary.lock.Unlock()
	}
	return deleted, deleteErr
}

// Verifies that LogicalContainerTunnelProxy retires a named runtime tunnel whose preparation response was lost,
// both when the tunnel is removed and before preparing it for a changed backend.
func TestV2LogicalContainerTunnelProxyRetiresLostPreparation(t *testing.T) {
	for _, scenario := range []string{"removed", "backend-changed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			boundary := &tunnelLostPreparationClient{
				prepared: make(map[string]int32), applied: make(chan struct{}), release: make(chan struct{}),
			}
			releasePreparation := sync.OnceFunc(func() { close(boundary.release) })
			defer releasePreparation()
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateTunnelControlClient: func(client dcptunproto.TunnelControlClient) dcptunproto.TunnelControlClient {
					boundary.TunnelControlClient = client
					return boundary
				},
			})
			require.NoError(t, server.Client.Create(ctx, proxy))
			select {
			case <-boundary.applied:
			case <-ctx.Done():
				t.Fatal("tunnel preparation was not applied")
			}
			require.True(t, environment.TestTunnelControlClient.HasPreparedTunnel("http"))
			if scenario == "removed" {
				require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerTunnelProxy) error {
					current.Spec.Tunnels = nil
					return server.Client.Update(updateCtx, current)
				}))
				releasePreparation()
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return len(current.Status.TunnelStatuses) == 0 && !environment.TestTunnelControlClient.HasPreparedTunnel("http"), nil
				})
			} else {
				host := &apiv2.LogicalService{}
				require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: "host"}, host))
				require.NoError(t, server.Client.Delete(ctx, host))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, host)
				port, portErr := networking.GetFreePort(commonapi.PortProtocolTCP, networking.IPv4LocalhostDefaultAddress, environment.Log)
				require.NoError(t, portErr)
				replacement := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: host.Name, Namespace: host.Namespace},
					Spec: apiv2.LogicalServiceSpec{Port: port}}
				require.NoError(t, server.Client.Create(ctx, replacement))
				allocated := waitObjectAssumesStateEx(t, ctx, server.Client, replacement.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return len(current.Status.EffectiveAddresses) > 0, nil
				})
				address, found, selectionErr := networking.SelectServiceAddress(allocated.Status.EffectiveAddresses,
					networking.IpVersionPreferenceNone, networking.GetIpVersionPreference())
				require.NoError(t, selectionErr)
				require.True(t, found)
				require.Equal(t, port, address.Port)
				environment.EnableTunnel(&dcptunproto.TunnelSpec{ServerAddress: stdproto.String(address.Address), ServerPort: stdproto.Int32(address.Port),
					ClientProxyPort: stdproto.Int32(address.Port)})
				releasePreparation()
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady &&
						current.Status.TunnelStatuses[0].ClientProxyPort == port, nil
				})
			}
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

// Verifies that LogicalContainerTunnelProxy observes server exit while its client is running, paused, or unknown,
// logs the server's stderr, reports Exited with Completed, withdraws endpoints, and stops the client backing.
func TestV2LogicalContainerTunnelProxyServerExit(t *testing.T) {
	for _, scenario := range []string{"running", "paused", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			loggedStderr := concurrency.NewValuePromise[string]()
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateLogger: func(base logr.Logger) logr.Logger {
					return logr.New(&stderrCaptureLogSink{inner: base.GetSink(), stderrFound: loggedStderr})
				},
			})
			const diagnostic = "tunnel server runtime diagnostic"
			installLogicalTunnelServer(t, ctx, environment, logicalTunnelServerBehavior{stderr: diagnostic})
			require.NoError(t, server.Client.Create(ctx, proxy))
			running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
					len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
			})
			endpoint := waitObjectAssumesStateEx(t, ctx, server.Client,
				types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID) + "-http"},
				func(current *apiv2.ServiceEndpoint) (bool, error) { return len(current.Spec.Addresses) > 0, nil })
			if scenario != "running" {
				runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				clientState, expectedReason := containers.ContainerStatusPaused, apiv2.LogicalContainerTunnelProxyReasonPaused
				if scenario == "unknown" {
					clientState, expectedReason = containers.ContainerStatus("test-unrecognized"), apiv2.LogicalContainerTunnelProxyReasonLookupFailed
				}
				require.NoError(t, runtime.SimulateContainerStatus(ctx, running.Status.ContainerID, clientState))
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return tunnelProxyReadyReason(current) == expectedReason, nil
				})
			}
			pid, pidErr := process.Int64_ToPidT(*running.Status.PID)
			require.NoError(t, pidErr)
			environment.SimulateProcessExit(t, pid, 1)
			select {
			case <-loggedStderr.Wait():
				require.Equal(t, diagnostic, loggedStderr.Get())
			case <-ctx.Done():
				t.Fatal("unexpected server exit did not log its stderr")
			}
			waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalContainerRef},
				func(current *apiv2.PhysicalContainer) (bool, error) { return current.Spec.Stop, nil })
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, endpoint)
			if scenario != "running" {
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonStopping && condition != nil &&
						strings.Contains(condition.Message, "Host server process exited with code 1"), nil
				})
				// The test runtime only auto-stops running containers.
				runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				require.NoError(t, runtime.SimulateContainerExit(ctx, running.Status.ContainerID, 0))
			}
			waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseExited && condition != nil &&
					condition.Reason == string(apiv2.LogicalContainerTunnelProxyReasonCompleted) &&
					strings.Contains(condition.Message, "Host server process exited with code 1") &&
					current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateFailed, nil
			})
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

// Verifies that deleting a LogicalContainerTunnelProxy while its host server has not reported control readiness
// cancels startup instead of waiting for the startup timeout, stops the server, and does not report FailedToStart.
func TestV2LogicalContainerTunnelProxyDeletionCancelsServerStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
		ConfigureLogicalTunnelProxy: func(config *controllers.LogicalContainerTunnelProxyReconcilerConfig) {
			// The startup timeout outlasts the test context, so only cancellation can end the readiness wait.
			config.ControlStartupTimeout = 10 * time.Minute
		},
	})
	installLogicalTunnelServer(t, ctx, environment, logicalTunnelServerBehavior{withholdReadiness: true})
	const hold = "test.dcp/hold-tunnel-proxy"
	proxy.Finalizers = []string{hold}
	require.NoError(t, server.Client.Create(ctx, proxy))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhasePending && condition != nil &&
			condition.Message == "Waiting for the host tunnel server to report control readiness.", nil
	})
	executable, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	executions := environment.TestProcessExecutor.FindAll([]string{executable, "tunnel-server"}, "", nil)
	require.Len(t, executions, 1)
	serverPID := int64(executions[0].PID)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	released := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return slices.Equal(current.Finalizers, []string{hold}), nil
	})
	require.NotEqual(t, apiv2.LogicalContainerTunnelProxyPhaseFailed, released.Status.Phase)
	require.NotEqual(t, apiv2.LogicalContainerTunnelProxyReasonFailedToStart, tunnelProxyReadyReason(released))
	waitLogicalTunnelServerStopped(t, ctx, environment, &serverPID)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerTunnelProxy) error {
		current.Finalizers = nil
		return server.Client.Update(updateCtx, current)
	}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that LogicalContainerTunnelProxy deletion retries a failed host server stop and keeps its finalizer
// until a stop attempt succeeds.
func TestV2LogicalContainerTunnelProxyRetriesServerStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	var stopBlocked atomic.Bool
	var stopAttempts atomic.Uint32
	stopBlocked.Store(true)
	installLogicalTunnelServer(t, ctx, environment, logicalTunnelServerBehavior{
		stopError: func(execution *internal_testutil.ProcessExecution) error {
			if stopBlocked.Load() {
				stopAttempts.Add(1)
				return errors.New("injected tunnel server stop failure")
			}
			select {
			case execution.Signal <- syscall.SIGTERM:
			default:
			}
			<-execution.ExecutionEnded
			return nil
		},
	})
	defer stopBlocked.Store(false)
	require.NoError(t, server.Client.Create(ctx, proxy))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning, nil
	})
	require.NoError(t, server.Client.Delete(ctx, proxy))
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
		return stopAttempts.Load() >= 2, nil
	}), "a failed server stop must be retried")
	require.NoError(t, server.Client.Get(ctx, proxy.NamespacedName(), &apiv2.LogicalContainerTunnelProxy{}),
		"deletion must wait for the host server to stop")
	stopBlocked.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

type tunnelImageDeletionClient struct {
	ctrl_client.Client
	blocked  atomic.Bool
	attempts atomic.Uint32
}

func (boundary *tunnelImageDeletionClient) Delete(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.DeleteOption) error {
	_, isImage := object.(*apiv2.PhysicalContainerImage)
	owner := metav1.GetControllerOf(object)
	if isImage && owner != nil && owner.Kind == "LogicalContainerTunnelProxy" && boundary.blocked.Load() {
		boundary.attempts.Add(1)
		return errors.New("injected tunnel image deletion failure")
	}
	return boundary.Client.Delete(ctx, object, options...)
}

// Verifies that deletion stops a previously running proxy and its host server even when its current state is unknown,
// and preserves earlier terminal outcomes. Image cleanup waits for the client container and its errors must not replace
// Exited/Stopped, Exited/Completed, or Failed/FailedToStart.
func TestV2LogicalContainerTunnelProxyDeletionLifecycle(t *testing.T) {
	for _, scenario := range []string{"running", "unknown", "completed", "failed-to-start"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var boundary *tunnelImageDeletionClient
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
					boundary = &tunnelImageDeletionClient{Client: client}
					boundary.blocked.Store(true)
					return boundary
				},
			})
			defer boundary.blocked.Store(false)
			if scenario == "failed-to-start" {
				executable, pathErr := dcppaths.GetDcpExePath()
				require.NoError(t, pathErr)
				environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
					Condition: internal_testutil.ProcessSearchCriteria{Command: []string{executable, "tunnel-server"}},
					StartupError: func(*internal_testutil.ProcessExecution) error {
						return errors.New("injected tunnel server launch failure")
					},
				})
			}
			require.NoError(t, server.Client.Create(ctx, proxy))
			initialPhase, initialReason := apiv2.LogicalContainerTunnelProxyPhaseRunning, apiv2.LogicalContainerTunnelProxyReasonRunning
			if scenario == "failed-to-start" {
				initialPhase, initialReason = apiv2.LogicalContainerTunnelProxyPhaseFailed, apiv2.LogicalContainerTunnelProxyReasonFailedToStart
			}
			initial := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return current.Status.Phase == initialPhase && tunnelProxyReadyReason(current) == initialReason, nil
			})
			if scenario == "completed" {
				pid, pidErr := process.Int64_ToPidT(*initial.Status.PID)
				require.NoError(t, pidErr)
				environment.SimulateProcessExit(t, pid, 1)
				initialPhase, initialReason = apiv2.LogicalContainerTunnelProxyPhaseExited, apiv2.LogicalContainerTunnelProxyReasonCompleted
				initial = waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return current.Status.Phase == initialPhase && tunnelProxyReadyReason(current) == initialReason, nil
				})
			}
			if scenario == "unknown" {
				runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				require.NoError(t, runtime.SimulateContainerStatus(ctx, initial.Status.ContainerID, containers.ContainerStatus("test-unrecognized")))
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseUnknown &&
						tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonLookupFailed, nil
				})
			}
			backingName := types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID)}
			const hold = "test.dcp/hold-tunnel-workload"
			require.NoError(t, retryOnConflictEx(ctx, server.Client, backingName, func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
				current.Finalizers = append(current.Finalizers, hold)
				return server.Client.Update(updateCtx, current)
			}))
			require.NoError(t, server.Client.Delete(ctx, proxy))
			waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalContainer) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			if initial.Status.PID != nil {
				waitLogicalTunnelServerStopped(t, ctx, environment, initial.Status.PID)
			}
			image := &apiv2.PhysicalContainerImage{}
			require.NoError(t, server.Client.Get(ctx, backingName, image))
			require.True(t, image.DeletionTimestamp.IsZero())
			require.Zero(t, boundary.attempts.Load())
			expectDeletionStop := scenario == "running" || scenario == "unknown"
			if expectDeletionStop {
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
						tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonStopping, nil
				})
				initialPhase, initialReason = apiv2.LogicalContainerTunnelProxyPhaseExited, apiv2.LogicalContainerTunnelProxyReasonStopped
			}
			if scenario == "unknown" {
				runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				require.NoError(t, runtime.SimulateContainerStatus(ctx, initial.Status.ContainerID, containers.ContainerStatusRunning))
			}
			require.NoError(t, retryOnConflictEx(ctx, server.Client, backingName, func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
				current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool { return value == hold })
				return server.Client.Update(updateCtx, current)
			}))
			preserved := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return boundary.attempts.Load() >= 2 && current.Status.Phase == initialPhase && tunnelProxyReadyReason(current) == initialReason, nil
			})
			if !expectDeletionStop {
				require.Equal(t, apimeta.FindStatusCondition(initial.Status.Conditions, string(apiv2.ConditionReady)).Message,
					apimeta.FindStatusCondition(preserved.Status.Conditions, string(apiv2.ConditionReady)).Message)
			}
			boundary.blocked.Store(false)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

// Verifies that namespace cleanup deletes the tunnel proxy and all owned physical backings without deadlocking their logical holds.
func TestV2LogicalContainerTunnelProxyNamespaceCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	require.NoError(t, server.Client.Create(ctx, proxy))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning, nil
	})
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: proxy.Namespace}}
	require.NoError(t, server.Client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, namespace)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

type tunnelCreateResponseLoss struct {
	ctrl_client.Client
	kind     string
	injected atomic.Bool
}

func (boundary *tunnelCreateResponseLoss) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := boundary.Client.Create(ctx, object, options...)
	matches := false
	switch object.(type) {
	case *apiv2.PhysicalContainerImage:
		matches = boundary.kind == "PhysicalContainerImage"
	case *apiv2.PhysicalContainer:
		matches = boundary.kind == "PhysicalContainer"
	}
	if createErr == nil && matches && boundary.injected.CompareAndSwap(false, true) {
		object.SetUID("")
		return errors.New("injected lost physical create response")
	}
	return createErr
}

type tunnelImageCreateRetryClient struct {
	ctrl_client.Client
	commitFirst bool
	committed   atomic.Bool
	released    atomic.Bool
	archives    chan string
}

func (boundary *tunnelImageCreateRetryClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	image, isImage := object.(*apiv2.PhysicalContainerImage)
	owner := metav1.GetControllerOf(object)
	if !isImage || owner == nil || owner.Kind != "LogicalContainerTunnelProxy" || boundary.released.Load() {
		return boundary.Client.Create(ctx, object, options...)
	}
	select {
	case boundary.archives <- image.Spec.Image.Build.ContextArchive.Source:
	default:
	}
	if boundary.commitFirst && boundary.committed.CompareAndSwap(false, true) {
		if createErr := boundary.Client.Create(ctx, object, options...); createErr != nil {
			return createErr
		}
		object.SetUID("")
	}
	return errors.New("injected tunnel image create response failure")
}

func (boundary *tunnelImageCreateRetryClient) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	listErr := boundary.Client.List(ctx, objects, options...)
	if listErr == nil && !boundary.released.Load() {
		listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
		if images, isImageList := objects.(*apiv2.PhysicalContainerImageList); isImageList && listOptions.FieldSelector != nil {
			images.Items = nil
		}
	}
	return listErr
}

// Verifies that LogicalContainerTunnelProxy reuses one test-session archive across uncertain image creates,
// preserves it during image acquisition, and delegates cleanup to the physical image after handoff.
func TestV2LogicalContainerTunnelProxyBuildArchiveLifecycle(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		commitFirst bool
		deleteEarly bool
	}{
		{name: "running"},
		{name: "running-after-lost-response", commitFirst: true},
		{name: "deleted-before-create", deleteEarly: true},
		{name: "deleted-after-lost-response", commitFirst: true, deleteEarly: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			buildRelease := make(chan struct{})
			releaseBuild := sync.OnceFunc(func() { close(buildRelease) })
			defer releaseBuild()
			var boundary *tunnelImageCreateRetryClient
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
					boundary = &tunnelImageCreateRetryClient{Client: client, commitFirst: scenario.commitFirst, archives: make(chan string, 2)}
					return boundary
				},
				DecorateContainerOrchestrator: func(inner containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
					return &archiveBuildOrchestrator{ContainerOrchestrator: inner, started: make(chan struct{}),
						cancelled: make(chan struct{}), release: buildRelease}
				},
			})
			defer boundary.released.Store(true)
			environment.addAfterShutdown(func() { require.NoDirExists(t, environment.SessionFolder) })
			require.NoError(t, server.Client.Create(ctx, proxy))
			var sources []string
			for len(sources) < 2 {
				select {
				case source := <-boundary.archives:
					sources = append(sources, source)
				case <-ctx.Done():
					t.Fatal("image declaration was not retried")
				}
			}
			require.Equal(t, sources[0], sources[1], "retries must reuse the prepared archive")
			require.Equal(t, environment.SessionFolder, filepath.Dir(sources[0]))
			require.FileExists(t, sources[0])
			archives, globErr := filepath.Glob(filepath.Join(environment.SessionFolder, "dcptun-build-context-*.tar"))
			require.NoError(t, globErr)
			require.Equal(t, []string{sources[0]}, archives)
			if !scenario.deleteEarly {
				boundary.released.Store(true)
				releaseBuild()
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					_, statErr := os.Stat(sources[0])
					if statErr != nil && !os.IsNotExist(statErr) {
						return false, statErr
					}
					return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
						len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady &&
						os.IsNotExist(statErr), nil
				})
			}
			require.NoError(t, server.Client.Delete(ctx, proxy))
			releaseBuild()
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
			require.NoFileExists(t, sources[0])
			remaining, remainingErr := filepath.Glob(filepath.Join(environment.SessionFolder, "dcptun-build-context-*.tar"))
			require.NoError(t, remainingErr)
			require.Empty(t, remaining)
		})
	}
}

// Verifies that LogicalContainerTunnelProxy recovers lost physical create responses without launching duplicate backings.
func TestV2LogicalContainerTunnelProxyLostCreateResponse(t *testing.T) {
	for _, kind := range []string{"PhysicalContainerImage", "PhysicalContainer"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var boundary *tunnelCreateResponseLoss
			server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				boundary = &tunnelCreateResponseLoss{Client: client, kind: kind}
				return boundary
			}})
			require.NoError(t, server.Client.Create(ctx, proxy))
			waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning, nil
			})
			require.True(t, boundary.injected.Load())
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

// Verifies that a tunnel proxy can acquire its image while waiting for a logical network and be deleted without creating a runtime container.
func TestV2LogicalContainerTunnelProxyPendingDeletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startupErr := StartTestEnvironment(t, ctx, LogicalContainerTunnelProxyController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pending-tunnel"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	proxy := &apiv2.LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerTunnelProxySpec{NetworkRef: "missing"}}
	require.NoError(t, server.Client.Create(ctx, proxy))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhasePending && current.Status.PhysicalImageRef != "", nil
	})
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
	var children apiv2.PhysicalContainerList
	require.NoError(t, server.Client.List(ctx, &children, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, children.Items)
	require.True(t, apierrors.IsNotFound(server.Client.Get(ctx, proxy.NamespacedName(), &apiv2.LogicalContainerTunnelProxy{})), fmt.Sprint(proxy.NamespacedName()))
}

// Verifies that client-container exit reports Exited with Completed, withdraws endpoints, and stops its host server.
func TestV2LogicalContainerTunnelProxyContainerExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning, nil
	})
	runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.NoError(t, runtime.SimulateContainerExit(ctx, running.Status.ContainerID, 1))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseExited && condition != nil &&
			condition.Reason == string(apiv2.LogicalContainerTunnelProxyReasonCompleted) &&
			strings.Contains(condition.Message, "Client container exited with code 1"), nil
	})
	waitLogicalTunnelServerStopped(t, ctx, environment, running.Status.PID)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that pausing the client container reports Running with Paused and Ready=False while keeping endpoints,
// then resuming the same container restores Ready=True and the normal Running reason.
func TestV2LogicalContainerTunnelProxyPausedContainer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonRunning, nil
	})
	endpointName := types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID) + "-http"}
	waitObjectAssumesStateEx(t, ctx, server.Client, endpointName, func(current *apiv2.ServiceEndpoint) (bool, error) {
		return len(current.Spec.Addresses) > 0, nil
	})
	runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.NoError(t, runtime.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusPaused))
	paused := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonPaused, nil
	})
	requireReadyCondition(t, paused.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerTunnelProxyReasonPaused)
	require.NoError(t, server.Client.Get(ctx, endpointName, &apiv2.ServiceEndpoint{}))
	require.NoError(t, runtime.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	resumed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonRunning, nil
	})
	requireReadyCondition(t, resumed.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalContainerTunnelProxyReasonRunning)
	require.Equal(t, running.Status.ContainerID, resumed.Status.ContainerID)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that a host server that fails to launch, or exits before reporting control readiness on stdout,
// reports FailedToStart with the startup failure and remains deletable with its owned physical container.
func TestV2LogicalContainerTunnelProxyServerLaunchFailure(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		failure internal_testutil.AutoExecution
		message string
	}{
		{
			name: "launch-error",
			failure: internal_testutil.AutoExecution{StartupError: func(*internal_testutil.ProcessExecution) error {
				return errors.New("injected tunnel server launch failure")
			}},
			message: "injected tunnel server launch failure",
		},
		{
			name:    "exit-before-ready",
			failure: internal_testutil.AutoExecution{RunCommand: func(*internal_testutil.ProcessExecution) int32 { return 3 }},
			message: "Host server process exited with code 3.",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
			executable, pathErr := dcppaths.GetDcpExePath()
			require.NoError(t, pathErr)
			scenario.failure.Condition = internal_testutil.ProcessSearchCriteria{Command: []string{executable, "tunnel-server"}}
			environment.TestProcessExecutor.InstallAutoExecution(scenario.failure)
			require.NoError(t, server.Client.Create(ctx, proxy))
			waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseFailed && condition != nil &&
					condition.Reason == string(apiv2.LogicalContainerTunnelProxyReasonFailedToStart) &&
					strings.Contains(condition.Message, scenario.message), nil
			})
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

// Verifies that a failed logical network reports FailedToStart without declaring its physical container.
func TestV2LogicalContainerTunnelProxyNetworkFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	_, runtimeErr := server.ContainerOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: "colliding-network"})
	require.NoError(t, runtimeErr)
	network := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "failed-network", Namespace: proxy.Namespace},
		Spec: apiv2.LogicalContainerNetworkSpec{NetworkName: "colliding-network", Network: &apiv2.LogicalContainerNetworkConfig{}}}
	require.NoError(t, server.Client.Create(ctx, network))
	waitObjectAssumesStateEx(t, ctx, server.Client, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseFailed, nil
	})
	proxy.Spec.NetworkRef = network.Name
	require.NoError(t, server.Client.Create(ctx, proxy))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseFailed &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonFailedToStart, nil
	})
	require.Empty(t, failed.Status.PhysicalContainerRef)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that absent physical backings report Failed with Missing and name every missing resource.
// Tunnel availability is withdrawn, a surviving client container is stopped, and the host server is stopped.
func TestV2LogicalContainerTunnelProxyMissingWhileRunning(t *testing.T) {
	for _, scenario := range []struct {
		kind    string
		message string
	}{
		{kind: "container", message: "The owned PhysicalContainer no longer exists."},
		{kind: "image", message: "The owned PhysicalContainerImage no longer exists."},
		{kind: "all", message: "The owned PhysicalContainer no longer exists. The owned PhysicalContainerImage no longer exists."},
	} {
		t.Run(scenario.kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var boundary *tunnelChildVisibilityClient
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
					boundary = &tunnelChildVisibilityClient{Client: client, kind: scenario.kind, created: make(chan ctrl_client.Object, 1)}
					return boundary
				},
			})
			defer boundary.hidden.Store(false)
			require.NoError(t, server.Client.Create(ctx, proxy))
			running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
					len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
			})
			boundary.hidden.Store(true)
			require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerTunnelProxy) error {
				current.Annotations = map[string]string{"test.dcp/reconcile": "missing"}
				return server.Client.Update(updateCtx, current)
			}))
			failed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseFailed &&
					tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonMissing &&
					current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateFailed, nil
			})
			require.Equal(t, scenario.message, apimeta.FindStatusCondition(failed.Status.Conditions, string(apiv2.ConditionReady)).Message)
			if scenario.kind != "all" && scenario.kind != "container" {
				waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalContainerRef},
					func(current *apiv2.PhysicalContainer) (bool, error) { return current.Spec.Stop, nil })
			}
			waitLogicalTunnelServerStopped(t, ctx, environment, running.Status.PID)
			boundary.hidden.Store(false)
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

// Verifies that lookup errors retry without overriding the proxy phase, while an observed Unknown backing reports Unknown.
// The same proxy pair recovers once the backing's runtime state becomes known.
func TestV2LogicalContainerTunnelProxyObservationRetries(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var boundary *tunnelChildVisibilityClient
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
		DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			boundary = &tunnelChildVisibilityClient{Client: client, kind: "container"}
			return boundary
		},
	})
	defer boundary.lookupError.Store(false)
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
	})
	boundary.lookupError.Store(true)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, proxy.NamespacedName(), func(updateCtx context.Context, current *apiv2.LogicalContainerTunnelProxy) error {
		current.Annotations = map[string]string{"test.dcp/reconcile": "lookup-error"}
		return server.Client.Update(updateCtx, current)
	}))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return boundary.lookupFailures.Load() >= 2 && current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonRunning, nil
	})
	boundary.lookupError.Store(false)
	endpointName := types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID) + "-http"}
	waitObjectAssumesStateEx(t, ctx, server.Client, endpointName, func(current *apiv2.ServiceEndpoint) (bool, error) {
		return current.DeletionTimestamp.IsZero() && len(current.Spec.Addresses) != 0, nil
	})
	runtime := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.NoError(t, runtime.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatus("test-unrecognized")))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseUnknown &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonLookupFailed, nil
	})
	require.NoError(t, runtime.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	recovered := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonRunning, nil
	})
	require.Equal(t, running.Status.ContainerID, recovered.Status.ContainerID)
	require.Equal(t, running.Status.PID, recovered.Status.PID)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that logical tunnel proxy reconciliation reports namespace admission waits and read errors
// without declaring physical resources, and remains deletable before controller data is initialized.
func TestV2LogicalContainerTunnelProxyNamespaceAdmission(t *testing.T) {
	for _, scenario := range []string{"missing", "lookup-error"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var namespaceClient *unavailableNamespaceClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalContainerTunnelProxyController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
					namespaceClient = &unavailableNamespaceClient{Client: client}
					return namespaceClient
				}})
			require.NoError(t, startupErr)
			namespaceClient.unavailable.Store(scenario == "lookup-error")
			defer namespaceClient.unavailable.Store(false)
			proxy := &apiv2.LogicalContainerTunnelProxy{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: "not-created"},
				Spec: apiv2.LogicalContainerTunnelProxySpec{NetworkRef: "network"}}
			require.NoError(t, server.Client.Create(ctx, proxy))
			expectedPhase, expectedReason := apiv2.LogicalContainerTunnelProxyPhasePending, apiv2.LogicalContainerTunnelProxyReasonStarting
			expectedMessage := "does not exist"
			if scenario == "lookup-error" {
				expectedPhase, expectedReason = apiv2.LogicalContainerTunnelProxyPhaseUnknown, apiv2.LogicalContainerTunnelProxyReasonLookupFailed
				expectedMessage = "Failed to get namespace"
			}
			waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == expectedPhase && condition != nil && condition.Reason == string(expectedReason) &&
					strings.Contains(condition.Message, expectedMessage), nil
			})
			for _, list := range []ctrl_client.ObjectList{&apiv2.PhysicalContainerList{}, &apiv2.PhysicalContainerImageList{}} {
				require.NoError(t, server.Client.List(ctx, list, ctrl_client.InNamespace(proxy.Namespace)))
				objects, extractErr := apimeta.ExtractList(list)
				require.NoError(t, extractErr)
				require.Empty(t, objects)
			}
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
		})
	}
}

type tunnelChildVisibilityClient struct {
	ctrl_client.Client
	kind           string
	hidden         atomic.Bool
	lookupError    atomic.Bool
	lookupFailures atomic.Uint32
	createdOnce    atomic.Bool
	created        chan ctrl_client.Object
	hiddenChild    chan struct{}
}

func (boundary *tunnelChildVisibilityClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	_, isImage := object.(*apiv2.PhysicalContainerImage)
	_, isContainer := object.(*apiv2.PhysicalContainer)
	owner := metav1.GetControllerOf(object)
	target := owner != nil && owner.Kind == "LogicalContainerTunnelProxy" &&
		((isImage && boundary.kind == "image") || (isContainer && boundary.kind == "container"))
	createErr := boundary.Client.Create(ctx, object, options...)
	if createErr != nil || !target || !boundary.hidden.Load() {
		return createErr
	}
	committed := object.DeepCopyObject().(ctrl_client.Object)
	if isImage {
		object.SetUID("")
	}
	if boundary.createdOnce.CompareAndSwap(false, true) {
		boundary.created <- committed
	}
	return nil
}

func (boundary *tunnelChildVisibilityClient) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if _, isContainer := objects.(*apiv2.PhysicalContainerList); isContainer && listOptions.FieldSelector != nil && boundary.lookupError.Load() {
		boundary.lookupFailures.Add(1)
		return errors.New("injected tunnel container lookup failure")
	}
	listErr := boundary.Client.List(ctx, objects, options...)
	if listErr != nil || !boundary.hidden.Load() {
		return listErr
	}
	if listOptions.FieldSelector == nil {
		return nil
	}
	childHidden := false
	switch children := objects.(type) {
	case *apiv2.PhysicalContainerList:
		if boundary.kind == "container" || boundary.kind == "all" {
			childHidden = len(children.Items) > 0
			children.Items = nil
		}
	case *apiv2.PhysicalContainerImageList:
		if boundary.kind == "image" || boundary.kind == "all" {
			childHidden = len(children.Items) > 0
			children.Items = nil
		}
	}
	if childHidden {
		select {
		case boundary.hiddenChild <- struct{}{}:
		default:
		}
	}
	return nil
}

// Verifies that LogicalContainerTunnelProxy reaches Running with its original owned backings after delayed visibility
// or incomplete Create responses, without duplicate physical resources, and remains deletable.
func TestV2LogicalContainerTunnelProxyRecoversBackingVisibility(t *testing.T) {
	for _, scenario := range []struct {
		name string
		kind string
	}{
		{name: "image", kind: "image"},
		{name: "container", kind: "container"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var boundary *tunnelChildVisibilityClient
			server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
					boundary = &tunnelChildVisibilityClient{Client: client, kind: scenario.kind,
						created: make(chan ctrl_client.Object, 1), hiddenChild: make(chan struct{}, 1)}
					boundary.hidden.Store(true)
					return boundary
				},
			})
			defer boundary.hidden.Store(false)
			require.NoError(t, server.Client.Create(ctx, proxy))
			var committed ctrl_client.Object
			select {
			case committed = <-boundary.created:
			case <-ctx.Done():
				t.Fatal("physical child was not created")
			}
			select {
			case <-boundary.hiddenChild:
			case <-ctx.Done():
				t.Fatal("physical child visibility was not delayed")
			}
			boundary.hidden.Store(false)
			waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
					len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
			})
			recovered := committed.DeepCopyObject().(ctrl_client.Object)
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(committed), recovered))
			require.Equal(t, committed.GetUID(), recovered.GetUID())
			require.True(t, metav1.IsControlledBy(recovered, proxy))
			for _, list := range []ctrl_client.ObjectList{&apiv2.PhysicalContainerImageList{}, &apiv2.PhysicalContainerList{}} {
				require.NoError(t, server.Client.List(ctx, list, ctrl_client.InNamespace(proxy.Namespace)))
				backings, extractErr := apimeta.ExtractList(list)
				require.NoError(t, extractErr)
				require.Len(t, backings, 1)
			}
			require.NoError(t, server.Client.Delete(ctx, proxy))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
			switch child := committed.(type) {
			case *apiv2.PhysicalContainerImage:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
			case *apiv2.PhysicalContainer:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
			}
		})
	}
}

// Verifies that a terminating network keeps tunnel preparation pending without declaring a container,
// and replacement of that logical network allows normal startup.
func TestV2LogicalContainerTunnelProxyWaitsForTerminatingNetwork(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	name := types.NamespacedName{Namespace: proxy.Namespace, Name: "network"}
	const hold = "test.dcp/hold-network"
	require.NoError(t, retryOnConflictEx(ctx, server.Client, name, func(updateCtx context.Context, current *apiv2.LogicalContainerNetwork) error {
		current.Finalizers = append(current.Finalizers, hold)
		return server.Client.Update(updateCtx, current)
	}))
	network := &apiv2.LogicalContainerNetwork{}
	require.NoError(t, server.Client.Get(ctx, name, network))
	require.NoError(t, server.Client.Delete(ctx, network))
	require.NoError(t, server.Client.Create(ctx, proxy))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhasePending && condition != nil &&
			strings.Contains(condition.Message, "is terminating"), nil
	})
	var containers apiv2.PhysicalContainerList
	require.NoError(t, server.Client.List(ctx, &containers, ctrl_client.InNamespace(proxy.Namespace)))
	require.Empty(t, containers.Items)
	require.NoError(t, retryOnConflictEx(ctx, server.Client, name, func(updateCtx context.Context, current *apiv2.LogicalContainerNetwork) error {
		current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool { return value == hold })
		return server.Client.Update(updateCtx, current)
	}))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, network)
	replacement := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
		Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}}}
	require.NoError(t, server.Client.Create(ctx, replacement))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning, nil
	})
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}
