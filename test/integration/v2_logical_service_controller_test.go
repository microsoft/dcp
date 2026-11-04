/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

type recordedServiceProxy struct {
	proxy.Proxy
	lifetimeCtx   context.Context
	effectivePort int32
	lock          sync.Mutex
	config        proxy.ProxyConfig
}

func (recorded *recordedServiceProxy) Configure(config proxy.ProxyConfig) error {
	configureErr := recorded.Proxy.Configure(config)
	if configureErr != nil {
		return configureErr
	}
	recorded.lock.Lock()
	recorded.config = config.Clone()
	recorded.lock.Unlock()
	return nil
}

func (recorded *recordedServiceProxy) EffectivePort() int32 {
	return recorded.effectivePort
}

func (recorded *recordedServiceProxy) configuration() proxy.ProxyConfig {
	recorded.lock.Lock()
	defer recorded.lock.Unlock()
	return recorded.config.Clone()
}

type serviceProxyRecorder struct {
	lock    sync.Mutex
	proxies []*recordedServiceProxy
	// Override reported ports to simulate distinct per-address allocation outcomes without sockets.
	effectivePorts []int32
}

func (recorder *serviceProxyRecorder) create(
	protocol commonapi.PortProtocol, address string, port int32, ctx context.Context, log logr.Logger,
) proxy.Proxy {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	reportedPort := port
	if len(recorder.proxies) < len(recorder.effectivePorts) {
		reportedPort = recorder.effectivePorts[len(recorder.proxies)]
	}
	recorded := &recordedServiceProxy{
		Proxy: ctrl_testutil.NewTestProxy(protocol, address, port, ctx, log), lifetimeCtx: ctx, effectivePort: reportedPort,
	}
	recorder.proxies = append(recorder.proxies, recorded)
	return recorded
}

func (recorder *serviceProxyRecorder) snapshot() []*recordedServiceProxy {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	return slices.Clone(recorder.proxies)
}

// Verifies that LogicalService allocates listeners before readiness, updates routes as endpoints
// change without replacing listeners, and cancels listeners on explicit or namespace deletion.
func TestV2LogicalServiceReadinessAndListenerLifetime(t *testing.T) {
	t.Parallel()
	for _, protocol := range []commonapi.PortProtocol{commonapi.PortProtocolTCP, commonapi.PortProtocolUDP} {
		for _, cleanup := range []string{"service", "namespace"} {
			t.Run(fmt.Sprintf("%s/%s", protocol, cleanup), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				recorder := &serviceProxyRecorder{}
				serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
					TestEnvironmentOptions{CreateLogicalServiceProxy: recorder.create})
				require.NoError(t, startupErr)
				apiClient := serverInfo.Client
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "service-lifetime"}}
				require.NoError(t, apiClient.Create(ctx, namespace))
				service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
					Spec: apiv2.LogicalServiceSpec{Protocol: protocol, Address: "127.0.0.1"}}
				require.NoError(t, apiClient.Create(ctx, service))
				pending := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhasePending && len(current.Status.EffectiveAddresses) == 1, nil
				})
				requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, "WaitingForEndpoints")
				addresses := slices.Clone(pending.Status.EffectiveAddresses)
				require.Equal(t, "127.0.0.1", addresses[0].Address)
				require.Positive(t, addresses[0].Port)
				listeners := recorder.snapshot()
				require.Len(t, listeners, 1)
				require.Equal(t, proxy.ProxyStateRunning, listeners[0].State())
				require.Empty(t, listeners[0].configuration().Endpoints)

				endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
					Spec: apiv2.ServiceEndpointSpec{ServiceRef: namespace.Name + "/" + service.Name,
						Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}, {Address: "::1", Port: 61001}}}}
				require.NoError(t, apiClient.Create(ctx, endpoint))
				ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
				})
				requireReadyCondition(t, ready.Status.Conditions, metav1.ConditionTrue, "ServiceReady")
				require.Equal(t, addresses, ready.Status.EffectiveAddresses)
				require.Equal(t, []proxy.Endpoint{{Address: "127.0.0.1", Port: 61000}, {Address: "::1", Port: 61001}},
					listeners[0].configuration().Endpoints)

				endpoint.Spec.Addresses = []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 62000}}
				require.NoError(t, apiClient.Update(ctx, endpoint))
				waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseReady &&
						slices.Equal(listeners[0].configuration().Endpoints, []proxy.Endpoint{{Address: "127.0.0.1", Port: 62000}}), nil
				})
				require.NoError(t, apiClient.Delete(ctx, endpoint))
				withoutEndpoints := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
				})
				requireReadyCondition(t, withoutEndpoints.Status.Conditions, metav1.ConditionFalse, "WaitingForEndpoints")
				require.Equal(t, addresses, withoutEndpoints.Status.EffectiveAddresses)
				require.Empty(t, listeners[0].configuration().Endpoints)
				require.NoError(t, listeners[0].lifetimeCtx.Err())
				require.Len(t, recorder.snapshot(), 1)

				if cleanup == "service" {
					require.NoError(t, apiClient.Delete(ctx, service))
					ctrl_testutil.WaitObjectDeleted[apiv2.LogicalService](t, ctx, apiClient, service)
				} else {
					require.NoError(t, apiClient.Delete(ctx, namespace))
					ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
				}
				select {
				case <-listeners[0].lifetimeCtx.Done():
				case <-ctx.Done():
					t.Fatal("service deletion did not cancel its listener")
				}
				require.NoError(t, ctx.Err())
				require.Equal(t, proxy.ProxyStateFinished, listeners[0].State())
				if cleanup == "service" {
					require.NoError(t, apiClient.Delete(ctx, namespace))
					ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
				}
			})
		}
	}
}

// Verifies that LogicalService publishes every available IPv4/IPv6 listener with its own
// reported effective port, rather than collapsing addresses or assuming a common port.
func TestV2LogicalServicePublishesAllListenerAddresses(t *testing.T) {
	t.Parallel()
	ips, lookupErr := networking.GetPreferredHostIps(networking.Localhost)
	require.NoError(t, lookupErr)
	ipv4, ipv6 := false, false
	for _, ip := range ips {
		ipv4 = ipv4 || networking.IsValidIPv4(ip)
		ipv6 = ipv6 || networking.IsValidIPv6(ip)
	}
	if !ipv4 || !ipv6 {
		t.Skip("preferred localhost addresses do not include both IPv4 and IPv6")
	}
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	recorder := &serviceProxyRecorder{}
	for addressIndex := range ips {
		recorder.effectivePorts = append(recorder.effectivePorts, int32(41000+addressIndex))
	}
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{CreateLogicalServiceProxy: recorder.create})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "multi-address-service"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name}}
	require.NoError(t, apiClient.Create(ctx, service))
	pending := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhasePending && len(current.Status.EffectiveAddresses) == len(ips), nil
	})
	listeners := recorder.snapshot()
	require.Len(t, listeners, len(ips))
	var expected []apiv2.ServiceAddress
	for _, listener := range listeners {
		expected = append(expected, apiv2.ServiceAddress{
			Address: networking.ToStandaloneAddress(listener.EffectiveAddress()), Port: listener.EffectivePort(),
		})
	}
	require.ElementsMatch(t, expected, pending.Status.EffectiveAddresses)
	require.NotEqual(t, expected[0].Port, expected[1].Port)
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 62000}}}}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
	})
	require.ElementsMatch(t, expected, ready.Status.EffectiveAddresses)
	for _, listener := range listeners {
		require.Equal(t, []proxy.Endpoint{{Address: "127.0.0.1", Port: 62000}}, listener.configuration().Endpoints)
	}
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
	for _, listener := range listeners {
		require.ErrorIs(t, listener.lifetimeCtx.Err(), context.Canceled)
		require.Equal(t, proxy.ProxyStateFinished, listener.State())
	}
	require.NoError(t, ctx.Err())
}

// Verifies that proxyless LogicalService retains its producer when another endpoint sorts earlier,
// projects changes to all selected addresses, and fails over only when that producer is deleted.
func TestV2LogicalServiceProxylessKeepsSelectedProducer(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	recorder := &serviceProxyRecorder{}
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{CreateLogicalServiceProxy: recorder.create})
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "proxyless-service"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{
			Address: "not-bound.invalid", Port: 12345, AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless,
		}}
	require.NoError(t, apiClient.Create(ctx, service))
	pending := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
	})
	require.Empty(t, pending.Status.EffectiveAddresses)
	require.Empty(t, pending.Status.ProxylessEndpointRef)
	selected := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "middle", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name,
			Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 64000}, {Address: "::1", Port: 64001}}}}
	require.NoError(t, apiClient.Create(ctx, selected))
	first := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.ProxylessEndpointRef == selected.Name && current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
	})
	require.Equal(t, selected.Spec.Addresses, first.Status.EffectiveAddresses)
	earlier := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "aaa", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name,
			Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 65000}, {Address: "::1", Port: 65001}}}}
	require.NoError(t, apiClient.Create(ctx, earlier))
	selected.Spec.Addresses = []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 64002}, {Address: "::1", Port: 64003}}
	selected.Finalizers = []string{"tests.dcp.microsoft.com/hold"}
	require.NoError(t, apiClient.Update(ctx, selected))
	updated := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.ProxylessEndpointRef == selected.Name && slices.Equal(current.Status.EffectiveAddresses, selected.Spec.Addresses), nil
	})
	requireReadyCondition(t, updated.Status.Conditions, metav1.ConditionTrue, "EndpointSelected")
	require.NoError(t, apiClient.Delete(ctx, selected))
	failover := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.ProxylessEndpointRef == earlier.Name, nil
	})
	require.Equal(t, earlier.Spec.Addresses, failover.Status.EffectiveAddresses)
	require.NoError(t, apiClient.Get(ctx, selected.NamespacedName(), selected))
	require.False(t, selected.DeletionTimestamp.IsZero())
	selected.Finalizers = nil
	require.NoError(t, apiClient.Update(ctx, selected))
	require.NoError(t, apiClient.Delete(ctx, earlier))
	empty := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhasePending && current.Status.ProxylessEndpointRef == "", nil
	})
	require.Empty(t, empty.Status.EffectiveAddresses)
	require.Empty(t, recorder.snapshot())
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
}
