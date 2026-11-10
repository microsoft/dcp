/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	stdproto "google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/dcptun"
	dcptunproto "github.com/microsoft/dcp/internal/dcptun/proto"
	"github.com/microsoft/dcp/internal/networking"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func installLogicalTunnelServer(t *testing.T, ctx context.Context, environment *TestEnvironmentInfo) {
	t.Helper()
	executable, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{executable, "tunnel-server"}},
		RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
			arguments := execution.Cmd.Args
			value := func(flag string) string {
				index := slices.Index(arguments, flag)
				if index < 0 || index+1 == len(arguments) {
					return ""
				}
				return arguments[index+1]
			}
			security := dcptun.TunnelProxySecurityConfig{
				CACertBase64: value("--ca-cert"), ServerCertBase64: value("--server-cert"), ServerKeyBase64: value("--server-key"),
			}
			tlsConfig, tlsErr := security.GetTlsConfig()
			if tlsErr != nil {
				t.Error(tlsErr)
				return 1
			}
			listener := dcptun.NewDialBackListener(ctx, value("--control-callback"), environment.Log)
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			select {
			case _, open := <-execution.Signal:
				if !open {
					t.Error("tunnel server signal channel closed unexpectedly")
				}
			case <-ctx.Done():
			}
			server.Stop()
			_ = listener.Close()
			if serveErr := <-done; serveErr != nil {
				t.Error(serveErr)
			}
			return 0
		},
	})
}

func startLogicalTunnelApplication(
	t *testing.T, ctx context.Context, options TestEnvironmentOptions,
) (*ctrl_testutil.ApiServerInfo, *TestEnvironmentInfo, *apiv2.LogicalContainerTunnelProxy) {
	t.Helper()
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalContainerTunnelProxyController|LogicalServiceController, t.Name(), t.TempDir(), options)
	require.NoError(t, startupErr)
	installLogicalTunnelServer(t, ctx, environment)
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

// Verifies that LogicalContainerTunnelProxy owns namespaced physical backings, honors baseImage,
// publishes Proxyless service endpoints, and supports tunnel removal and re-addition without replacing the proxy pair.
func TestV2LogicalContainerTunnelProxyLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	require.NoError(t, server.Client.Create(ctx, proxy))
	running := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			len(current.Status.TunnelStatuses) == 1 && current.Status.TunnelStatuses[0].State == apiv2.LogicalContainerTunnelStateReady, nil
	})
	require.NotEmpty(t, running.Status.ContainerID)
	require.NotNil(t, running.Status.PID)
	container, image, hostProcess := &apiv2.PhysicalContainer{}, &apiv2.PhysicalContainerImage{}, &apiv2.PhysicalProcess{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalContainerRef}, container))
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID)}, image))
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalProcessRef}, hostProcess))
	for _, child := range []ctrl_client.Object{container, image, hostProcess} {
		require.True(t, metav1.IsControlledBy(child, proxy))
		require.Contains(t, child.GetFinalizers(), controllers.LogicalBackingFinalizer)
	}
	require.Equal(t, []string{proxy.Spec.BaseImage}, image.Spec.Image.Build.BaseImages)
	require.Equal(t, proxy.Spec.Aliases, container.Spec.Container.Networks[0].Aliases)
	require.Contains(t, hostProcess.Spec.Process.Args, "--control-callback")
	endpointName := types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID) + "-http"}
	endpoint := waitObjectAssumesStateEx(t, ctx, server.Client, endpointName, func(current *apiv2.ServiceEndpoint) (bool, error) {
		return len(current.Spec.Addresses) > 0, nil
	})
	require.True(t, metav1.IsControlledBy(endpoint, proxy))
	clientService := waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: proxy.Namespace, Name: "client"}, func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
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
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, hostProcess)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, endpoint)
	require.NoError(t, server.Client.Get(ctx, clientService.NamespacedName(), &apiv2.LogicalService{}))
}

// Verifies that LogicalContainerTunnelProxy observes server exit while its client is running, paused, or unknown,
// reports Exited with Completed, withdraws endpoints, and stops the client backing.
func TestV2LogicalContainerTunnelProxyServerExit(t *testing.T) {
	for _, scenario := range []string{"running", "paused", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
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

// Verifies that deletion stops a running proxy through its normal lifecycle states and preserves earlier terminal outcomes.
// Image cleanup waits for both workloads and its errors must not replace Exited/Stopped, Exited/Completed, or Failed/FailedToStart.
func TestV2LogicalContainerTunnelProxyDeletionLifecycle(t *testing.T) {
	for _, scenario := range []string{"running", "completed", "failed-to-start"} {
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
			backingName := types.NamespacedName{Namespace: proxy.Namespace, Name: "logical-" + string(proxy.UID)}
			const hold = "test.dcp/hold-tunnel-workload"
			require.NoError(t, retryOnConflictEx(ctx, server.Client, backingName, func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
				current.Finalizers = append(current.Finalizers, hold)
				return server.Client.Update(updateCtx, current)
			}))
			require.NoError(t, retryOnConflictEx(ctx, server.Client, backingName, func(updateCtx context.Context, current *apiv2.PhysicalProcess) error {
				current.Finalizers = append(current.Finalizers, hold)
				return server.Client.Update(updateCtx, current)
			}))
			require.NoError(t, server.Client.Delete(ctx, proxy))
			waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalContainer) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalProcess) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			image := &apiv2.PhysicalContainerImage{}
			require.NoError(t, server.Client.Get(ctx, backingName, image))
			require.True(t, image.DeletionTimestamp.IsZero())
			require.Zero(t, boundary.attempts.Load())
			if scenario == "running" {
				waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
						tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonStopping, nil
				})
				initialPhase, initialReason = apiv2.LogicalContainerTunnelProxyPhaseExited, apiv2.LogicalContainerTunnelProxyReasonStopped
			}
			require.NoError(t, retryOnConflictEx(ctx, server.Client, backingName, func(updateCtx context.Context, current *apiv2.PhysicalContainer) error {
				current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool { return value == hold })
				return server.Client.Update(updateCtx, current)
			}))
			require.NoError(t, retryOnConflictEx(ctx, server.Client, backingName, func(updateCtx context.Context, current *apiv2.PhysicalProcess) error {
				current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool { return value == hold })
				return server.Client.Update(updateCtx, current)
			}))
			preserved := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
				return boundary.attempts.Load() >= 2 && current.Status.Phase == initialPhase && tunnelProxyReadyReason(current) == initialReason, nil
			})
			if scenario != "running" {
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
	case *apiv2.PhysicalProcess:
		matches = boundary.kind == "PhysicalProcess"
	}
	if createErr == nil && matches && boundary.injected.CompareAndSwap(false, true) {
		object.SetUID("")
		return errors.New("injected lost physical create response")
	}
	return createErr
}

// Verifies that LogicalContainerTunnelProxy recovers lost physical create responses without launching duplicate backings.
func TestV2LogicalContainerTunnelProxyLostCreateResponse(t *testing.T) {
	for _, kind := range []string{"PhysicalContainerImage", "PhysicalContainer", "PhysicalProcess"} {
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
	server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
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
	waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalProcessRef},
		func(current *apiv2.PhysicalProcess) (bool, error) { return current.Spec.Stop, nil })
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that pausing the client container reports Running with Paused and keeps endpoints,
// then resuming the same container returns the proxy to its normal Running reason.
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
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonPaused, nil
	})
	require.NoError(t, server.Client.Get(ctx, endpointName, &apiv2.ServiceEndpoint{}))
	require.NoError(t, runtime.SimulateContainerStatus(ctx, running.Status.ContainerID, containers.ContainerStatusRunning))
	resumed := waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseRunning &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonRunning, nil
	})
	require.Equal(t, running.Status.ContainerID, resumed.Status.ContainerID)
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
}

// Verifies that failed server-process launch reports FailedToStart and remains deletable with its owned physical container.
func TestV2LogicalContainerTunnelProxyServerLaunchFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{})
	executable, pathErr := dcppaths.GetDcpExePath()
	require.NoError(t, pathErr)
	environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{Command: []string{executable, "tunnel-server"}},
		StartupError: func(*internal_testutil.ProcessExecution) error {
			return errors.New("injected tunnel server launch failure")
		},
	})
	require.NoError(t, server.Client.Create(ctx, proxy))
	waitObjectAssumesStateEx(t, ctx, server.Client, proxy.NamespacedName(), func(current *apiv2.LogicalContainerTunnelProxy) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerTunnelProxyPhaseFailed &&
			tunnelProxyReadyReason(current) == apiv2.LogicalContainerTunnelProxyReasonFailedToStart, nil
	})
	require.NoError(t, server.Client.Delete(ctx, proxy))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, proxy)
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
// Tunnel availability is withdrawn and surviving workloads are stopped.
func TestV2LogicalContainerTunnelProxyMissingWhileRunning(t *testing.T) {
	for _, scenario := range []struct {
		kind    string
		message string
	}{
		{kind: "container", message: "The owned PhysicalContainer no longer exists."},
		{kind: "image", message: "The owned PhysicalContainerImage no longer exists."},
		{kind: "process", message: "The owned PhysicalProcess no longer exists."},
		{kind: "all", message: "The owned PhysicalContainer no longer exists. The owned PhysicalContainerImage no longer exists. The owned PhysicalProcess no longer exists."},
	} {
		t.Run(scenario.kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var boundary *tunnelChildVisibilityClient
			server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
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
			if scenario.kind != "all" && scenario.kind != "process" {
				waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: proxy.Namespace, Name: running.Status.PhysicalProcessRef},
					func(current *apiv2.PhysicalProcess) (bool, error) { return current.Spec.Stop, nil })
			}
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
			boundary = &tunnelChildVisibilityClient{Client: client, kind: "process"}
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
			for _, list := range []ctrl_client.ObjectList{&apiv2.PhysicalContainerList{}, &apiv2.PhysicalContainerImageList{}, &apiv2.PhysicalProcessList{}} {
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
	kind             string
	undecodedProcess bool
	hidden           atomic.Bool
	lookupError      atomic.Bool
	lookupFailures   atomic.Uint32
	createdOnce      atomic.Bool
	created          chan ctrl_client.Object
	hiddenChild      chan struct{}
}

func (boundary *tunnelChildVisibilityClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	_, isImage := object.(*apiv2.PhysicalContainerImage)
	_, isContainer := object.(*apiv2.PhysicalContainer)
	_, isProcess := object.(*apiv2.PhysicalProcess)
	owner := metav1.GetControllerOf(object)
	target := owner != nil && owner.Kind == "LogicalContainerTunnelProxy" &&
		((isImage && boundary.kind == "image") || (isContainer && boundary.kind == "container") || (isProcess && boundary.kind == "process"))
	createErr := boundary.Client.Create(ctx, object, options...)
	if createErr != nil || !target || !boundary.hidden.Load() {
		return createErr
	}
	committed := object.DeepCopyObject().(ctrl_client.Object)
	if isImage || boundary.undecodedProcess {
		object.SetUID("")
	}
	if boundary.createdOnce.CompareAndSwap(false, true) {
		boundary.created <- committed
	}
	if isProcess && !boundary.undecodedProcess {
		return errors.New("injected create response error after decoding the physical process UID")
	}
	return nil
}

func (boundary *tunnelChildVisibilityClient) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if _, isProcess := objects.(*apiv2.PhysicalProcessList); isProcess && listOptions.FieldSelector != nil && boundary.lookupError.Load() {
		boundary.lookupFailures.Add(1)
		return errors.New("injected tunnel process lookup failure")
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
	case *apiv2.PhysicalProcessList:
		if boundary.kind == "process" || boundary.kind == "all" {
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
		name             string
		kind             string
		undecodedProcess bool
	}{
		{name: "image", kind: "image"},
		{name: "container", kind: "container"},
		{name: "process-create-error", kind: "process"},
		{name: "process-undecoded-success", kind: "process", undecodedProcess: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var boundary *tunnelChildVisibilityClient
			server, _, proxy := startLogicalTunnelApplication(t, ctx, TestEnvironmentOptions{
				DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
					boundary = &tunnelChildVisibilityClient{Client: client, kind: scenario.kind, undecodedProcess: scenario.undecodedProcess,
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
			for _, list := range []ctrl_client.ObjectList{&apiv2.PhysicalContainerImageList{}, &apiv2.PhysicalContainerList{}, &apiv2.PhysicalProcessList{}} {
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
			case *apiv2.PhysicalProcess:
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
