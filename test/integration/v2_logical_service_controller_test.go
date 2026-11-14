/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

type recordedServiceProxy struct {
	proxy.Proxy
	lifetimeCtx        context.Context
	effectivePort      int32
	lock               sync.Mutex
	config             proxy.ProxyConfig
	stopProxy          context.CancelFunc
	configurationCalls atomic.Uint32
	healthChecks       atomic.Uint32
	failConfiguration  atomic.Bool
	failStart          bool
}

type serviceEndpointLookupFailureClient struct {
	ctrl_client.Client
	fail                atomic.Bool
	conflictAllocation  atomic.Bool
	conflictEndpointRef atomic.Pointer[string]
}

func (apiClient *serviceEndpointLookupFailureClient) List(
	ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) error {
	if _, endpoints := objects.(*apiv2.ServiceEndpointList); endpoints && apiClient.fail.Load() {
		return errors.New("service endpoint lookup unavailable")
	}
	return apiClient.Client.List(ctx, objects, options...)
}

func (apiClient *serviceEndpointLookupFailureClient) Status() ctrl_client.SubResourceWriter {
	return &serviceAllocationConflictWriter{SubResourceWriter: apiClient.Client.Status(), client: apiClient}
}

type serviceAllocationConflictWriter struct {
	ctrl_client.SubResourceWriter
	client *serviceEndpointLookupFailureClient
}

func (writer *serviceAllocationConflictWriter) Patch(
	ctx context.Context, object ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption,
) error {
	expectedEndpoint := writer.client.conflictEndpointRef.Load()
	if service, logical := object.(*apiv2.LogicalService); logical && len(service.Status.EffectiveAddresses) > 0 &&
		(expectedEndpoint == nil || service.Status.ProxylessEndpointRef == *expectedEndpoint) &&
		writer.client.conflictAllocation.CompareAndSwap(true, false) {
		writer.client.fail.Store(true)
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "logicalservices"},
			service.Name, errors.New("injected allocation status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func (recorded *recordedServiceProxy) Start() error {
	if recorded.failStart {
		return errors.New("service proxy startup unavailable")
	}
	return recorded.Proxy.Start()
}

func (recorded *recordedServiceProxy) Configure(config proxy.ProxyConfig) error {
	recorded.configurationCalls.Add(1)
	if recorded.failConfiguration.Load() {
		return errors.New("service proxy configuration unavailable")
	}
	configureErr := recorded.Proxy.Configure(config)
	if configureErr != nil {
		return configureErr
	}
	recorded.lock.Lock()
	recorded.config = config.Clone()
	recorded.lock.Unlock()
	return nil
}

func (recorded *recordedServiceProxy) State() proxy.ProxyState {
	recorded.healthChecks.Add(1)
	return recorded.Proxy.State()
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
	createProxy    proxy.ProxyFactory
	startFailures  int
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
	factory := recorder.createProxy
	if factory == nil {
		factory = ctrl_testutil.NewTestProxy
	}
	proxyCtx, stopProxy := context.WithCancel(ctx)
	recorded := &recordedServiceProxy{
		Proxy: factory(protocol, address, port, proxyCtx, log), lifetimeCtx: proxyCtx,
		effectivePort: reportedPort, stopProxy: stopProxy,
	}
	if recorder.startFailures > 0 {
		recorder.startFailures--
		recorded.failStart = true
	}
	recorder.proxies = append(recorder.proxies, recorded)
	return recorded
}

func (recorder *serviceProxyRecorder) snapshot() []*recordedServiceProxy {
	recorder.lock.Lock()
	defer recorder.lock.Unlock()
	return slices.Clone(recorder.proxies)
}

type serviceReconciliationObserver struct {
	ctrl_client.Client
	lastServiceRead atomic.Int64
}

func (observer *serviceReconciliationObserver) Get(ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, isService := object.(*apiv2.LogicalService); isService {
		observer.lastServiceRead.Store(time.Now().UnixNano())
	}
	return observer.Client.Get(ctx, key, object, options...)
}

func waitControllerIdle(t *testing.T, ctx context.Context, lastRead *atomic.Int64) {
	t.Helper()
	require.NotZero(t, lastRead.Load())
	quietPeriod := 2 * (max(testReconciliationTiming.StandardDelay, testReconciliationTiming.RetryDelay,
		testReconciliationTiming.MonitoringDelay) + testReconciliationTiming.MaxDebounceDelay)
	// Allow queued events to drain, then observe silence across two configured scheduling/debounce cycles.
	require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, false, func(context.Context) (bool, error) {
		return time.Since(time.Unix(0, lastRead.Load())) >= quietPeriod, nil
	}))
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
					return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current) &&
						len(current.Status.EffectiveAddresses) == 1, nil
				})
				requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
				addresses := slices.Clone(pending.Status.EffectiveAddresses)
				require.Equal(t, "127.0.0.1", addresses[0].Address)
				require.Positive(t, addresses[0].Port)
				listeners := recorder.snapshot()
				require.Len(t, listeners, 1)
				require.Equal(t, proxy.ProxyStateRunning, listeners[0].State())
				require.Empty(t, listeners[0].configuration().Endpoints)
				require.Equal(t, uint32(1), listeners[0].configurationCalls.Load())

				endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
					Spec: apiv2.ServiceEndpointSpec{ServiceRef: namespace.Name + "/" + service.Name,
						Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}, {Address: "[::1]", Port: 61001}}}}
				require.NoError(t, apiClient.Create(ctx, endpoint))
				ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return logicalServiceReady(current), nil
				})
				requireReadyCondition(t, ready.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalServiceReasonReady)
				require.Equal(t, addresses, ready.Status.EffectiveAddresses)
				require.Equal(t, []proxy.Endpoint{{Address: "127.0.0.1", Port: 61000}, {Address: "[::1]", Port: 61001}},
					listeners[0].configuration().Endpoints)

				endpoint.Spec.Addresses = []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 62000}}
				endpoint = recreateServiceEndpoint(t, ctx, apiClient, endpoint)
				waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return logicalServiceReady(current) &&
						slices.Equal(listeners[0].configuration().Endpoints, []proxy.Endpoint{{Address: "127.0.0.1", Port: 62000}}), nil
				})
				require.NoError(t, apiClient.Delete(ctx, endpoint))
				withoutEndpoints := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current), nil
				})
				requireReadyCondition(t, withoutEndpoints.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
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

// Verifies that LogicalService remains Pending during failed initial namespace checks and checks readiness only before initializing service state.
// Later namespace read failures do not block endpoint updates or service deletion.
func TestV2LogicalServiceChecksNamespaceOnlyBeforeInitialization(t *testing.T) {
	t.Parallel()
	for _, mode := range []apiv2.ServiceAddressAllocationMode{"", apiv2.ServiceAddressAllocationModeProxyless} {
		name := "listeners"
		if mode == apiv2.ServiceAddressAllocationModeProxyless {
			name = "proxyless"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			recorder := &serviceProxyRecorder{}
			var decorated *unavailableNamespaceClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					CreateLogicalServiceProxy: recorder.create,
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						decorated = &unavailableNamespaceClient{Client: apiClient}
						return decorated
					},
				})
			require.NoError(t, startupErr)
			apiClient := server.Client
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "service-initial-admission"}}
			require.NoError(t, apiClient.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, apiClient, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
				Spec: apiv2.ServiceEndpointSpec{ServiceRef: "service", Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}}}}
			require.NoError(t, apiClient.Create(ctx, endpoint))
			decorated.unavailable.Store(true)
			defer decorated.unavailable.Store(false)
			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
				Spec: apiv2.LogicalServiceSpec{Address: "127.0.0.1", AddressAllocationMode: mode}}
			require.NoError(t, apiClient.Create(ctx, service))
			unavailable := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return len(current.Status.Conditions) > 0 &&
					strings.Contains(current.Status.Conditions[0].Message, "injected namespace read failure"), nil
			})
			require.Equal(t, apiv2.LogicalServicePhasePending, unavailable.Status.Phase)
			requireReadyCondition(t, unavailable.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonStarting)
			require.Empty(t, unavailable.Status.EffectiveAddresses)
			require.Empty(t, recorder.snapshot())
			decorated.unavailable.Store(false)
			running := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return logicalServiceReady(current), nil
			})
			listeners := recorder.snapshot()
			decorated.unavailable.Store(true)
			endpoint.Spec.Addresses[0].Port++
			endpoint = recreateServiceEndpoint(t, ctx, apiClient, endpoint)
			updated := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				if !logicalServiceReady(current) {
					return false, nil
				}
				if mode == apiv2.ServiceAddressAllocationModeProxyless {
					return slices.Equal(current.Status.EffectiveAddresses, endpoint.Spec.Addresses), nil
				}
				return slices.Equal(listeners[0].configuration().Endpoints,
					[]proxy.Endpoint{{Address: "127.0.0.1", Port: endpoint.Spec.Addresses[0].Port}}), nil
			})
			if mode == apiv2.ServiceAddressAllocationModeProxyless {
				require.Empty(t, recorder.snapshot())
			} else {
				require.Len(t, listeners, 1)
				require.Same(t, listeners[0], recorder.snapshot()[0])
				require.Equal(t, running.Status.EffectiveAddresses, updated.Status.EffectiveAddresses)
			}
			require.NoError(t, apiClient.Delete(ctx, service))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, service)
			decorated.unavailable.Store(false)
			require.NoError(t, apiClient.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
		})
	}
}

// Verifies that periodic and metadata-only reconciliations preserve an established UDP forwarding
// stream, while a changed endpoint configuration is applied once to the existing listener.
func TestV2LogicalServicePreservesUDPStreamAcrossReconciliation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	recorder := &serviceProxyRecorder{createProxy: proxy.NewRuntimeProxy}
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{CreateLogicalServiceProxy: recorder.create})
	require.NoError(t, startupErr)
	apiClient := server.Client
	backend, backendErr := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, backendErr)
	defer func() { require.NoError(t, backend.Close()) }()
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.NoError(t, backend.SetDeadline(deadline))
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unchanged-udp-routes"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{Protocol: commonapi.PortProtocolUDP, Address: "127.0.0.1"}}
	require.NoError(t, apiClient.Create(ctx, service))
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{
			Address: "127.0.0.1", Port: int32(backend.LocalAddr().(*net.UDPAddr).Port),
		}}}}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return logicalServiceReady(current), nil
	})
	listeners := recorder.snapshot()
	require.Len(t, listeners, 1)
	listener := listeners[0]
	front := ready.Status.EffectiveAddresses[0]
	connection, connectionErr := net.Dial("udp", networking.AddressAndPort(front.Address, front.Port))
	require.NoError(t, connectionErr)
	defer func() { require.NoError(t, connection.Close()) }()
	require.NoError(t, connection.SetDeadline(deadline))
	_, requestErr := connection.Write([]byte("request"))
	require.NoError(t, requestErr)
	buffer := make([]byte, 32)
	received, backendClient, receiveErr := backend.ReadFrom(buffer)
	require.NoError(t, receiveErr)
	require.Equal(t, "request", string(buffer[:received]))
	configurationCount, initialHealthChecks := listener.configurationCalls.Load(), listener.healthChecks.Load()
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, false, func(context.Context) (bool, error) {
		return listener.healthChecks.Load() >= initialHealthChecks+2, nil
	}))
	require.Equal(t, configurationCount, listener.configurationCalls.Load())
	healthChecks := listener.healthChecks.Load()
	require.NoError(t, apiClient.Patch(ctx, service,
		ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"test":"metadata-only"}}}`))))
	endpoint.Annotations = map[string]string{"test": "metadata-only"}
	require.NoError(t, apiClient.Update(ctx, endpoint))
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, false, func(context.Context) (bool, error) {
		return listener.healthChecks.Load() > healthChecks, nil
	}))
	require.Equal(t, configurationCount, listener.configurationCalls.Load())
	_, replyErr := backend.WriteTo([]byte("reply"), backendClient)
	require.NoError(t, replyErr)
	responseSize, responseErr := connection.Read(buffer)
	require.NoError(t, responseErr)
	require.Equal(t, "reply", string(buffer[:responseSize]))

	nextBackend, nextBackendErr := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, nextBackendErr)
	defer func() { require.NoError(t, nextBackend.Close()) }()
	endpoint.Spec.Addresses[0].Port = int32(nextBackend.LocalAddr().(*net.UDPAddr).Port)
	endpoint = recreateServiceEndpoint(t, ctx, apiClient, endpoint)
	waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return logicalServiceReady(current) &&
			slices.Equal(listener.configuration().Endpoints, []proxy.Endpoint{{
				Address: "127.0.0.1", Port: endpoint.Spec.Addresses[0].Port,
			}}), nil
	})
	require.GreaterOrEqual(t, listener.configurationCalls.Load(), configurationCount+1)
	require.Len(t, recorder.snapshot(), 1)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		return listener.Proxy.State() == proxy.ProxyStateFinished, nil
	}))
}

// Verifies that LogicalService excludes stopped listeners without replacing them, preserves healthy
// listeners and their routes, stops polling after complete loss, and allocates only after service recreation.
func TestV2LogicalServiceDoesNotReplaceStoppedListeners(t *testing.T) {
	t.Parallel()
	for _, protocol := range []commonapi.PortProtocol{commonapi.PortProtocolTCP, commonapi.PortProtocolUDP} {
		for _, address := range []string{"127.0.0.1", networking.Localhost} {
			t.Run(string(protocol)+"/"+address, func(t *testing.T) {
				t.Parallel()
				if address == networking.Localhost {
					ips, lookupErr := networking.GetPreferredHostIps(address)
					require.NoError(t, lookupErr)
					if len(ips) < 2 {
						t.Skip("multiple localhost listener addresses are unavailable")
					}
				}
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				recorder := &serviceProxyRecorder{}
				var observer *serviceReconciliationObserver
				server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
					TestEnvironmentOptions{
						CreateLogicalServiceProxy: recorder.create,
						DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
							observer = &serviceReconciliationObserver{Client: apiClient}
							return observer
						},
					})
				require.NoError(t, startupErr)
				apiClient := server.Client
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "replacement-listener"}}
				require.NoError(t, apiClient.Create(ctx, namespace))
				service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
					Spec: apiv2.LogicalServiceSpec{Protocol: protocol, Address: address}}
				require.NoError(t, apiClient.Create(ctx, service))
				endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
					Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}}}}
				require.NoError(t, apiClient.Create(ctx, endpoint))
				ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return logicalServiceReady(current), nil
				})
				originals := recorder.snapshot()
				require.NotEmpty(t, originals)
				configurationCounts := make([]uint32, len(originals))
				for listenerIndex, listener := range originals {
					configurationCounts[listenerIndex] = listener.configurationCalls.Load()
				}
				original := originals[0]
				original.stopProxy()
				expectedAddresses := slices.Clone(ready.Status.EffectiveAddresses[1:])
				expectedPhase := apiv2.LogicalServicePhaseFailed
				degraded := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == expectedPhase &&
						slices.Equal(current.Status.EffectiveAddresses, expectedAddresses), nil
				})
				requireReadyCondition(t, degraded.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonMissing)
				require.ErrorIs(t, original.lifetimeCtx.Err(), context.Canceled)
				require.Len(t, recorder.snapshot(), len(originals))
				for healthyIndex, healthy := range originals[1:] {
					require.NoError(t, healthy.lifetimeCtx.Err())
					require.Equal(t, proxy.ProxyStateRunning, healthy.State())
					require.Equal(t, configurationCounts[healthyIndex+1], healthy.configurationCalls.Load())
				}

				previousChecks := original.healthChecks.Load()
				endpoint.Spec.Addresses[0].Port++
				endpoint = recreateServiceEndpoint(t, ctx, apiClient, endpoint)
				waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					for _, healthy := range originals[1:] {
						if !slices.Equal(healthy.configuration().Endpoints,
							[]proxy.Endpoint{{Address: "127.0.0.1", Port: endpoint.Spec.Addresses[0].Port}}) {
							return false, nil
						}
					}
					return original.healthChecks.Load() > previousChecks && current.Status.Phase == expectedPhase &&
						slices.Equal(current.Status.EffectiveAddresses, expectedAddresses), nil
				})
				require.Len(t, recorder.snapshot(), len(originals))
				require.Equal(t, configurationCounts[0], original.configurationCalls.Load())

				for _, remaining := range originals[1:] {
					remaining.stopProxy()
				}
				failed := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseFailed && len(current.Status.EffectiveAddresses) == 0, nil
				})
				requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonMissing)
				waitControllerIdle(t, ctx, &observer.lastServiceRead)
				require.Len(t, recorder.snapshot(), len(originals))

				require.NoError(t, apiClient.Delete(ctx, service))
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, service)
				recreated := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: service.Name, Namespace: namespace.Name}, Spec: service.Spec}
				require.NoError(t, apiClient.Create(ctx, recreated))
				recovered := waitObjectAssumesStateEx(t, ctx, apiClient, recreated.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return logicalServiceReady(current) &&
						len(current.Status.EffectiveAddresses) == len(originals), nil
				})
				require.NotEqual(t, ready.UID, recovered.UID)
				require.Len(t, recorder.snapshot(), len(originals)*2)
				require.NoError(t, apiClient.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
			})
		}
	}
}

// Verifies that LogicalService preserves startup or observed allocation during failed endpoint reads,
// including initial/replacement status conflicts, and clears confirmed withdrawn proxyless selections.
func TestV2LogicalServicePreservesLifecycleDuringEndpointLookupFailure(t *testing.T) {
	t.Parallel()
	t.Run("startup", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		var decorated *serviceEndpointLookupFailureClient
		resources := createServiceEndpointResources(t, ctx, commonapi.PortProtocolTCP, TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				decorated = &serviceEndpointLookupFailureClient{Client: apiClient}
				decorated.fail.Store(true)
				return decorated
			},
		})
		defer decorated.fail.Store(false)
		pending := waitObjectAssumesStateEx(t, ctx, resources.client, resources.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			return !logicalServiceReady(current) && len(current.Status.Conditions) > 0 &&
				current.Status.Conditions[0].Message == "service endpoint lookup unavailable", nil
		})
		require.Equal(t, apiv2.LogicalServicePhasePending, pending.Status.Phase)
		requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonStarting)
		decorated.fail.Store(false)
		resources.requireReady(t, ctx)
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resources.service.Namespace}}
		require.NoError(t, resources.client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, resources.client, namespace)
	})
	t.Run("allocation-status-conflict", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		var decorated *serviceEndpointLookupFailureClient
		resources := createServiceEndpointResources(t, ctx, commonapi.PortProtocolTCP, TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				decorated = &serviceEndpointLookupFailureClient{Client: apiClient}
				decorated.conflictAllocation.Store(true)
				return decorated
			},
		})
		defer decorated.fail.Store(false)
		allocated := waitObjectAssumesStateEx(t, ctx, resources.client, resources.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			return len(current.Status.Conditions) > 0 &&
				current.Status.Conditions[0].Message == "service endpoint lookup unavailable", nil
		})
		require.False(t, decorated.conflictAllocation.Load())
		require.Equal(t, apiv2.LogicalServicePhaseRunning, allocated.Status.Phase)
		requireReadyCondition(t, allocated.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
		listeners := resources.recorder.snapshot()
		require.Len(t, listeners, 1)
		listener := listeners[0]
		require.Equal(t, []apiv2.ServiceAddress{{
			Address: listener.EffectiveAddress(), Port: listener.EffectivePort(),
		}}, allocated.Status.EffectiveAddresses)
		require.Positive(t, allocated.Status.EffectiveAddresses[0].Port)
		require.NoError(t, listener.lifetimeCtx.Err())
		decorated.fail.Store(false)
		recovered, recoveredListener := resources.requireReady(t, ctx)
		require.Same(t, listener, recoveredListener)
		require.Equal(t, allocated.Status.EffectiveAddresses, recovered.Status.EffectiveAddresses)
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resources.service.Namespace}}
		require.NoError(t, resources.client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, resources.client, namespace)
	})
	t.Run("proxyless-allocation-status-conflict", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		t.Cleanup(cancel)
		var decorated *serviceEndpointLookupFailureClient
		server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
			TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				decorated = &serviceEndpointLookupFailureClient{Client: apiClient}
				decorated.conflictAllocation.Store(true)
				return decorated
			}})
		require.NoError(t, startupErr)
		defer decorated.fail.Store(false)
		apiClient := server.Client
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "proxyless-allocation-conflict"}}
		require.NoError(t, apiClient.Create(ctx, namespace))
		service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
			Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
		selected := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "selected", Namespace: namespace.Name},
			Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}}}}
		require.NoError(t, apiClient.Create(ctx, selected))
		require.NoError(t, apiClient.Create(ctx, service))
		allocated := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
			return ready != nil && ready.Message == "service endpoint lookup unavailable", nil
		})
		require.False(t, decorated.conflictAllocation.Load())
		require.Equal(t, apiv2.LogicalServicePhaseRunning, allocated.Status.Phase)
		requireReadyCondition(t, allocated.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
		require.Equal(t, selected.Name, allocated.Status.ProxylessEndpointRef)
		require.Equal(t, selected.Spec.Addresses, allocated.Status.EffectiveAddresses)
		decorated.fail.Store(false)
		waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			return logicalServiceReady(current) && current.Status.ProxylessEndpointRef == selected.Name, nil
		})

		replacement := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: namespace.Name},
			Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61001}}}}
		require.NoError(t, apiClient.Create(ctx, replacement))
		replacementRef := replacement.Name
		decorated.conflictEndpointRef.Store(&replacementRef)
		decorated.conflictAllocation.Store(true)
		require.NoError(t, apiClient.Delete(ctx, selected))
		replaced := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
			return ready != nil && ready.Message == "service endpoint lookup unavailable", nil
		})
		require.False(t, decorated.conflictAllocation.Load())
		require.Equal(t, apiv2.LogicalServicePhaseRunning, replaced.Status.Phase)
		requireReadyCondition(t, replaced.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
		require.Equal(t, replacement.Name, replaced.Status.ProxylessEndpointRef)
		require.Equal(t, replacement.Spec.Addresses, replaced.Status.EffectiveAddresses)
		decorated.fail.Store(false)
		waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			return logicalServiceReady(current) && current.Status.ProxylessEndpointRef == replacement.Name &&
				slices.Equal(current.Status.EffectiveAddresses, replacement.Spec.Addresses), nil
		})
		require.NoError(t, apiClient.Delete(ctx, replacement))
		waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			return current.Status.Phase == apiv2.LogicalServicePhasePending && current.Status.ProxylessEndpointRef == "" &&
				len(current.Status.EffectiveAddresses) == 0, nil
		})
		decorated.fail.Store(true)
		require.NoError(t, apiClient.Patch(ctx, service,
			ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"test-lookup":"empty-selection"}}}`))))
		empty := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
			return ready != nil && ready.Message == "service endpoint lookup unavailable", nil
		})
		require.Equal(t, apiv2.LogicalServicePhasePending, empty.Status.Phase)
		requireReadyCondition(t, empty.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonStarting)
		require.Empty(t, empty.Status.ProxylessEndpointRef)
		require.Empty(t, empty.Status.EffectiveAddresses)
		decorated.fail.Store(false)
		require.NoError(t, apiClient.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
	})
	t.Run("running", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		var decorated *serviceEndpointLookupFailureClient
		resources := createServiceEndpointResources(t, ctx, commonapi.PortProtocolTCP, TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				decorated = &serviceEndpointLookupFailureClient{Client: apiClient}
				return decorated
			},
		})
		defer decorated.fail.Store(false)
		original, listener := resources.requireReady(t, ctx)
		decorated.fail.Store(true)
		require.NoError(t, resources.client.Patch(ctx, resources.service,
			ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"test-lookup":"retry"}}}`))))
		unavailable := waitObjectAssumesStateEx(t, ctx, resources.client, resources.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
			return !logicalServiceReady(current) && len(current.Status.Conditions) > 0 &&
				current.Status.Conditions[0].Message == "service endpoint lookup unavailable", nil
		})
		require.Equal(t, apiv2.LogicalServicePhaseRunning, unavailable.Status.Phase)
		requireReadyCondition(t, unavailable.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
		require.Equal(t, original.Status.EffectiveAddresses, unavailable.Status.EffectiveAddresses)
		require.NoError(t, listener.lifetimeCtx.Err())
		decorated.fail.Store(false)
		recovered, recoveredListener := resources.requireReady(t, ctx)
		require.Same(t, listener, recoveredListener)
		require.Equal(t, original.Status.EffectiveAddresses, recovered.Status.EffectiveAddresses)
		namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resources.service.Namespace}}
		require.NoError(t, resources.client.Delete(ctx, namespace))
		ctrl_testutil.WaitObjectDeleted(t, ctx, resources.client, namespace)
	})
}

// Verifies that LogicalService retries a route-configuration failure while remaining Running
// without changing its frontend address or cancelling the listener lifetime.
func TestV2LogicalServiceRetriesConfigurationWithoutReplacingListener(t *testing.T) {
	t.Parallel()
	for _, protocol := range []commonapi.PortProtocol{commonapi.PortProtocolTCP, commonapi.PortProtocolUDP} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			resources := createServiceEndpointResources(t, ctx, protocol, TestEnvironmentOptions{})
			original, listener := resources.requireReady(t, ctx)
			listener.failConfiguration.Store(true)
			resources.endpoint.Spec.Addresses[0].Port++
			resources.endpoint = recreateServiceEndpoint(t, ctx, resources.client, resources.endpoint)
			unavailable := waitObjectAssumesStateEx(t, ctx, resources.client, resources.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return !logicalServiceReady(current) && len(current.Status.Conditions) > 0 &&
					strings.Contains(current.Status.Conditions[0].Message, "configuration unavailable"), nil
			})
			require.Equal(t, apiv2.LogicalServicePhaseRunning, unavailable.Status.Phase)
			requireReadyCondition(t, unavailable.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
			require.Equal(t, original.Status.EffectiveAddresses, unavailable.Status.EffectiveAddresses)
			require.NoError(t, listener.lifetimeCtx.Err())
			listener.failConfiguration.Store(false)
			recovered, recoveredListener := resources.requireReady(t, ctx)
			require.Same(t, listener, recoveredListener)
			require.Equal(t, original.Status.EffectiveAddresses, recovered.Status.EffectiveAddresses)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resources.service.Namespace}}
			require.NoError(t, resources.client.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, resources.client, namespace)
		})
	}
}

type serviceEndpointResources struct {
	client   ctrl_client.Client
	service  *apiv2.LogicalService
	endpoint *apiv2.ServiceEndpoint
	recorder *serviceProxyRecorder
}

func createServiceEndpointResources(
	t *testing.T, ctx context.Context, protocol commonapi.PortProtocol, options TestEnvironmentOptions,
) *serviceEndpointResources {
	t.Helper()
	recorder := &serviceProxyRecorder{}
	options.CreateLogicalServiceProxy = recorder.create
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(), options)
	require.NoError(t, startupErr)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "service-endpoint-retry"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{Protocol: protocol, Address: "127.0.0.1"}}
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}}}}
	require.NoError(t, server.Client.Create(ctx, endpoint))
	require.NoError(t, server.Client.Create(ctx, service))
	return &serviceEndpointResources{client: server.Client, service: service, endpoint: endpoint, recorder: recorder}
}

func (resources *serviceEndpointResources) requireReady(t *testing.T, ctx context.Context) (*apiv2.LogicalService, *recordedServiceProxy) {
	t.Helper()
	ready := waitObjectAssumesStateEx(t, ctx, resources.client, resources.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return logicalServiceReady(current), nil
	})
	listeners := resources.recorder.snapshot()
	require.Len(t, listeners, 1)
	require.Equal(t, []proxy.Endpoint{{Address: "127.0.0.1", Port: resources.endpoint.Spec.Addresses[0].Port}}, listeners[0].configuration().Endpoints)
	require.NoError(t, listeners[0].lifetimeCtx.Err())
	require.Equal(t, proxy.ProxyStateRunning, listeners[0].State())
	return ready, listeners[0]
}

// Verifies that LogicalService retries failed initial listener startup before publishing its
// frontend address and retains the successfully started listener on later reconciliations.
func TestV2LogicalServiceRetriesInitialListenerStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	recorder := &serviceProxyRecorder{startFailures: 1}
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{CreateLogicalServiceProxy: recorder.create})
	require.NoError(t, startupErr)
	apiClient := server.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "retry-listener-startup"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"}}
	require.NoError(t, apiClient.Create(ctx, service))
	pending := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return len(current.Status.EffectiveAddresses) == 1, nil
	})
	require.Equal(t, apiv2.LogicalServicePhaseRunning, pending.Status.Phase)
	requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonAllocated)
	listeners := recorder.snapshot()
	require.Len(t, listeners, 2)
	require.ErrorIs(t, listeners[0].lifetimeCtx.Err(), context.Canceled)
	require.NoError(t, listeners[1].lifetimeCtx.Err())
	require.Positive(t, pending.Status.EffectiveAddresses[0].Port)
	healthChecks := listeners[1].healthChecks.Load()
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, false, func(context.Context) (bool, error) {
		return listeners[1].healthChecks.Load() >= healthChecks+2, nil
	}))
	require.Len(t, recorder.snapshot(), 2)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
}

// Verifies that LogicalService stops polling after exhausting listener startup, retains the final
// startup error across endpoint watch events, and makes no further allocation attempts.
func TestV2LogicalServiceStopsRetryingExhaustedListenerStartup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	recorder := &serviceProxyRecorder{startFailures: controllers.MaxServiceStartAttempts}
	var observer *serviceReconciliationObserver
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, LogicalServiceController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			CreateLogicalServiceProxy: recorder.create,
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				observer = &serviceReconciliationObserver{Client: apiClient}
				return observer
			},
		})
	require.NoError(t, startupErr)
	apiClient := server.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "exhausted-listener-startup"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"}}
	require.NoError(t, apiClient.Create(ctx, service))
	for attempt := 1; attempt <= controllers.MaxServiceStartAttempts; attempt++ {
		require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, false, func(context.Context) (bool, error) {
			return len(recorder.snapshot()) >= attempt, nil
		}))
		if attempt < controllers.MaxServiceStartAttempts {
			require.NoError(t, apiClient.Patch(ctx, service, ctrl_client.RawPatch(types.MergePatchType,
				fmt.Appendf(nil, `{"metadata":{"annotations":{"retry-check":"%d"}}}`, attempt))))
		}
	}
	failed := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonFailedToStart)
	failureMessage := "start service listener: service proxy startup unavailable"
	require.Equal(t, failureMessage, failed.Status.Conditions[0].Message)
	previousRead := observer.lastServiceRead.Load()
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 61000}}}}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return observer.lastServiceRead.Load() > previousRead && current.Status.Phase == apiv2.LogicalServicePhaseFailed &&
			len(current.Status.Conditions) > 0 && current.Status.Conditions[0].Message == failureMessage, nil
	})
	waitControllerIdle(t, ctx, &observer.lastServiceRead)
	require.Len(t, recorder.snapshot(), controllers.MaxServiceStartAttempts)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
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
		return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current) &&
			len(current.Status.EffectiveAddresses) == len(ips), nil
	})
	listeners := recorder.snapshot()
	require.Len(t, listeners, len(ips))
	var expected []apiv2.ServiceAddress
	for _, listener := range listeners {
		expected = append(expected, apiv2.ServiceAddress{
			Address: listener.EffectiveAddress(), Port: listener.EffectivePort(),
		})
	}
	require.ElementsMatch(t, expected, pending.Status.EffectiveAddresses)
	require.NotEqual(t, expected[0].Port, expected[1].Port)
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 62000}}}}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return logicalServiceReady(current), nil
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

// Verifies that proxyless LogicalService retains its producer until deletion, clears allocation
// when no endpoints remain, and becomes ready again when a replacement endpoint appears.
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
			Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 64000}, {Address: "[::1]", Port: 64001}}}}
	require.NoError(t, apiClient.Create(ctx, selected))
	first := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.ProxylessEndpointRef == selected.Name && logicalServiceReady(current), nil
	})
	require.Equal(t, selected.Spec.Addresses, first.Status.EffectiveAddresses)
	earlier := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "aaa", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name,
			Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 65000}, {Address: "[::1]", Port: 65001}}}}
	require.NoError(t, apiClient.Create(ctx, earlier))
	selected.Finalizers = []string{"tests.dcp.microsoft.com/hold"}
	require.NoError(t, apiClient.Update(ctx, selected))
	updated := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.ProxylessEndpointRef == selected.Name && slices.Equal(current.Status.EffectiveAddresses, selected.Spec.Addresses), nil
	})
	requireReadyCondition(t, updated.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalServiceReasonReady)
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
		return current.Status.Phase == apiv2.LogicalServicePhasePending && !logicalServiceReady(current) &&
			current.Status.ProxylessEndpointRef == "", nil
	})
	require.Empty(t, empty.Status.EffectiveAddresses)
	requireReadyCondition(t, empty.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalServiceReasonStarting)
	require.NoError(t, apiClient.Create(ctx, &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 63000}}}}))
	reallocated := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.ProxylessEndpointRef == "replacement" && logicalServiceReady(current), nil
	})
	require.Equal(t, []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 63000}}, reallocated.Status.EffectiveAddresses)
	requireReadyCondition(t, reallocated.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalServiceReasonReady)
	require.Empty(t, recorder.snapshot())
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
}
