/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/nettest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/proxy"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/ports"
	"github.com/microsoft/dcp/pkg/testutil"
)

func reservationDatabase(t *testing.T, environment *TestEnvironmentInfo) *sql.DB {
	t.Helper()
	db, openErr := sql.Open("sqlite", environment.StateStore.Path())
	require.NoError(t, openErr)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

func waitPortReservationCount(t *testing.T, ctx context.Context, db *sql.DB, expected int) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(ctx context.Context) (bool, error) {
		var count int
		queryErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM port_allocations").Scan(&count)
		return count == expected, queryErr
	}))
}

func requirePortReservationBinding(t *testing.T, ctx context.Context, db *sql.DB, binding ports.Binding) {
	t.Helper()
	var protocol string
	var ipBytes []byte
	var port int32
	require.NoError(t, db.QueryRowContext(ctx, "SELECT protocol, ip, port FROM port_allocations").
		Scan(&protocol, &ipBytes, &port))
	ip, validIP := netip.AddrFromSlice(ipBytes)
	require.True(t, validIP)
	require.Equal(t, binding, ports.Binding{Protocol: protocol, IP: ip, Port: port})
}

// Verifies that dcp start-apiserver configures SQLite-backed port allocation before
// kubeconfig creation, so strict state-store mode starts and reserves its actual listener.
func TestApiServerUsesStateStorePortAllocator(t *testing.T) {
	t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
	t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26120")
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	sessionFolder, sessionErr := testutil.CreateTestSessionDir()
	require.NoError(t, sessionErr)
	server, startupErr := ctrl_testutil.StartApiServer(ctx, ctrl_testutil.ApiServerFlagsNone,
		testutil.NewLogForTesting(t.Name()), sessionFolder)
	require.NoError(t, startupErr)
	t.Cleanup(server.Dispose)
	db, openErr := sql.Open("sqlite", filepath.Join(sessionFolder, "state.sqlite3"))
	require.NoError(t, openErr)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	serverURL, urlErr := url.Parse(server.ClientConfig.Host)
	require.NoError(t, urlErr)
	ip, ipErr := netip.ParseAddr(serverURL.Hostname())
	require.NoError(t, ipErr)
	port, portErr := strconv.Atoi(serverURL.Port())
	require.NoError(t, portErr)
	require.GreaterOrEqual(t, port, 26100)
	require.LessOrEqual(t, port, 26120)
	waitPortReservationCount(t, ctx, db, 1)
	requirePortReservationBinding(t, ctx, db, ports.Binding{Protocol: "TCP", IP: ip, Port: int32(port)})
	var ownerPID int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT owner_pid FROM port_allocations").Scan(&ownerPID))
	require.Equal(t, int64(server.ApiServerPID), ownerPID)
}

// Verifies that Executable template allocation and Endpoint publication share the wildcard
// bind claim, while exposing a loopback client address and defaulting omitted protocols to TCP.
func TestExecutableTemplatePortReservationUsesBindAddress(t *testing.T) {
	for _, testCase := range []struct {
		bindAddress     string
		endpointAddress string
		protocol        commonapi.PortProtocol
	}{
		{networking.IPv4AllInterfaceAddress, networking.IPv4LocalhostDefaultAddress, ""},
		{networking.IPv6AllInterfaceAddress, networking.IPv6LocalhostDefaultAddress, commonapi.PortProtocolTCP},
	} {
		t.Run(testCase.bindAddress, func(t *testing.T) {
			if testCase.bindAddress == networking.IPv6AllInterfaceAddress && !nettest.SupportsIPv6() {
				t.Skip("IPv6 is unavailable")
			}
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				ServiceController|ExecutableController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{ConfigurePortAllocator: true})
			require.NoError(t, startupErr)
			t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
			t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26120")
			db := reservationDatabase(t, environment)
			service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "producer"},
				Spec: apiv1.ServiceSpec{Protocol: testCase.protocol, AddressAllocationMode: apiv1.AddressAllocationModeProxyless}}
			require.NoError(t, server.Client.Create(ctx, service))
			executable := &apiv1.Executable{ObjectMeta: metav1.ObjectMeta{Name: "producer",
				Annotations: map[string]string{commonapi.ServiceProducerAnnotation: fmt.Sprintf(
					`[{"serviceName":"producer","address":%q}]`, testCase.bindAddress)}},
				Spec: apiv1.ExecutableSpec{ExecutablePath: "wildcard-template-producer",
					Args: []string{`{{portForServing "producer"}}`}}}
			require.NoError(t, server.Client.Create(ctx, executable))
			running := waitObjectAssumesStateEx(t, ctx, server.Client, executable.NamespacedName(), func(current *apiv1.Executable) (bool, error) {
				return current.Status.State == apiv1.ExecutableStateRunning, nil
			})
			ready := waitObjectAssumesStateEx(t, ctx, server.Client, service.NamespacedName(), func(current *apiv1.Service) (bool, error) {
				return current.Status.State == apiv1.ServiceStateReady, nil
			})
			require.Len(t, running.Status.EffectiveArgs, 1)
			require.Equal(t, fmt.Sprint(ready.Status.EffectivePort), running.Status.EffectiveArgs[0])
			var endpoints apiv1.EndpointList
			require.NoError(t, server.Client.List(ctx, &endpoints))
			require.Len(t, endpoints.Items, 1)
			require.Equal(t, testCase.endpointAddress, endpoints.Items[0].Spec.Address)
			waitPortReservationCount(t, ctx, db, 1)
			requirePortReservationBinding(t, ctx, db, ports.Binding{Protocol: "TCP",
				IP: netip.MustParseAddr(networking.ToStandaloneAddress(testCase.bindAddress)), Port: ready.Status.EffectivePort})
			require.NoError(t, server.Client.Delete(ctx, executable))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, executable)
			waitPortReservationCount(t, ctx, db, 0)
		})
	}
}

// Verifies that Container Endpoint reservations use the configured or inspected host bind
// address before it is rewritten to a loopback address for clients.
func TestContainerProducerPortReservationUsesBindAddress(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		hostIP   string
		hostPort int32
	}{
		{"fixed-wildcard", networking.IPv4AllInterfaceAddress, 26102},
		{"fixed-unspecified", "", 26102},
		{"dynamic-wildcard", networking.IPv4AllInterfaceAddress, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				ServiceController|ContainerController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{ConfigurePortAllocator: true})
			require.NoError(t, startupErr)
			t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
			t.Setenv(networking.DCP_PORT_ALLOCATION_ALLOW_EPHEMERAL_OVERLAP, "true")
			db := reservationDatabase(t, environment)
			service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "producer"},
				Spec: apiv1.ServiceSpec{AddressAllocationMode: apiv1.AddressAllocationModeProxyless}}
			require.NoError(t, server.Client.Create(ctx, service))
			container := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: "producer",
				Annotations: map[string]string{commonapi.ServiceProducerAnnotation: `[{"serviceName":"producer","port":8080}]`}},
				Spec: apiv1.ContainerSpec{Image: "producer-image",
					Ports: []apiv1.ContainerPort{{ContainerPort: 8080, HostPort: testCase.hostPort, HostIP: testCase.hostIP}}}}
			require.NoError(t, server.Client.Create(ctx, container))
			ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, container)
			ready := waitObjectAssumesStateEx(t, ctx, server.Client, service.NamespacedName(), func(current *apiv1.Service) (bool, error) {
				return current.Status.State == apiv1.ServiceStateReady, nil
			})
			var endpoints apiv1.EndpointList
			require.NoError(t, server.Client.List(ctx, &endpoints))
			require.Len(t, endpoints.Items, 1)
			require.Equal(t, networking.IPv4LocalhostDefaultAddress, endpoints.Items[0].Spec.Address)
			waitPortReservationCount(t, ctx, db, 1)
			requirePortReservationBinding(t, ctx, db, ports.Binding{Protocol: "TCP",
				IP: netip.MustParseAddr(networking.IPv4AllInterfaceAddress), Port: ready.Status.EffectivePort})
			require.NoError(t, server.Client.Delete(ctx, container))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, container)
			waitPortReservationCount(t, ctx, db, 0)
		})
	}
}

// Verifies that Executable template claims are released after confirmed startup failure
// and deletion while waiting for a template dependency, without Endpoint publication.
func TestExecutableTemplatePortReservationsReleasedBeforePublication(t *testing.T) {
	for _, outcome := range []string{"startup-failure", "deletion-while-templating"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				ServiceController|ExecutableController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{ConfigurePortAllocator: true})
			require.NoError(t, startupErr)
			t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
			t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26120")
			db := reservationDatabase(t, environment)
			service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "producer"},
				Spec: apiv1.ServiceSpec{AddressAllocationMode: apiv1.AddressAllocationModeProxyless}}
			require.NoError(t, server.Client.Create(ctx, service))
			var claimsAtFailure atomic.Int32
			environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
				Condition: internal_testutil.ProcessSearchCriteria{Command: []string{"template-port-startup-failure"}},
				StartupError: func(_ *internal_testutil.ProcessExecution) error {
					var claimCount int
					if queryErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM port_allocations").Scan(&claimCount); queryErr != nil {
						return queryErr
					}
					claimsAtFailure.Store(int32(claimCount))
					return errors.New("injected Executable startup failure")
				},
			})
			executable := &apiv1.Executable{ObjectMeta: metav1.ObjectMeta{Name: "producer",
				Annotations: map[string]string{commonapi.ServiceProducerAnnotation: `[{"serviceName":"producer","address":"0.0.0.0"}]`}},
				Spec: apiv1.ExecutableSpec{ExecutablePath: "template-port-startup-failure",
					Args: []string{`{{portForServing "producer"}}`}}}
			if outcome == "deletion-while-templating" {
				executable.Spec.Args = append(executable.Spec.Args, `{{portFor "missing"}}`)
			}
			require.NoError(t, server.Client.Create(ctx, executable))
			if outcome == "startup-failure" {
				waitObjectAssumesStateEx(t, ctx, server.Client, executable.NamespacedName(), func(current *apiv1.Executable) (bool, error) {
					return current.Status.State == apiv1.ExecutableStateFailedToStart, nil
				})
				require.Equal(t, int32(1), claimsAtFailure.Load())
				waitPortReservationCount(t, ctx, db, 0)
			} else {
				waitPortReservationCount(t, ctx, db, 1)
			}
			require.NoError(t, server.Client.Delete(ctx, executable))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, executable)
			waitPortReservationCount(t, ctx, db, 0)
		})
	}
}

// Verifies that both Service APIs default unspecified protocols to TCP, stop fixed/dynamic
// listeners, and finish deletion despite a failed SQLite release without retrying it.
func TestServicePortReservationReleaseDoesNotBlockDeletion(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		for _, fixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fixed=%t", version, fixed), func(t *testing.T) {
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				recorder := &serviceProxyRecorder{}
				server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
					ServiceController|LogicalServiceController, t.Name(), t.TempDir(),
					TestEnvironmentOptions{ConfigurePortAllocator: true,
						CreateServiceProxy: recorder.create, CreateLogicalServiceProxy: recorder.create})
				require.NoError(t, startupErr)
				t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
				t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26120")
				db := reservationDatabase(t, environment)
				var port int32
				if fixed {
					port = 26101
				}
				var service ctrl_client.Object
				if version == "v1" {
					service = &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "release"},
						Spec: apiv1.ServiceSpec{Address: "127.0.0.1", Port: port}}
				} else {
					namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "reservations"}}
					require.NoError(t, server.Client.Create(ctx, namespace))
					service = &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: namespace.Name},
						Spec: apiv2.LogicalServiceSpec{Address: "127.0.0.1", Port: port}}
				}
				require.NoError(t, server.Client.Create(ctx, service))
				waitPortReservationCount(t, ctx, db, 1)
				require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
					proxies := recorder.snapshot()
					return len(proxies) > 0 && proxies[0].State() == proxy.ProxyStateRunning, nil
				}))
				_, triggerErr := db.ExecContext(ctx, `CREATE TRIGGER fail_release BEFORE DELETE ON port_allocations
					BEGIN SELECT RAISE(FAIL, 'injected release failure'); END`)
				require.NoError(t, triggerErr)
				require.NoError(t, server.Client.Delete(ctx, service))
				switch typed := service.(type) {
				case *apiv1.Service:
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, typed)
				case *apiv2.LogicalService:
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, typed)
				}
				for _, recorded := range recorder.snapshot() {
					require.ErrorIs(t, recorded.lifetimeCtx.Err(), context.Canceled)
				}
				waitPortReservationCount(t, ctx, db, 1)
				_, dropErr := db.ExecContext(ctx, "DROP TRIGGER fail_release")
				require.NoError(t, dropErr)
				waitPortReservationCount(t, ctx, db, 1)
			})
		}
	}
}

type endpointReservationFailureClient struct {
	ctrl_client.Client
	failures atomic.Int32
	fail     atomic.Bool
}

func (apiClient *endpointReservationFailureClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	if _, endpoint := object.(*apiv1.Endpoint); endpoint && apiClient.fail.Load() {
		apiClient.failures.Add(1)
		return errors.New("injected Endpoint creation failure")
	}
	return apiClient.Client.Create(ctx, object, options...)
}

// Verifies that workload-owned claims survive Service deletion, are released without looking
// the Service up again, and remain owned while Endpoint publication fails.
func TestEndpointPortReservationOwnedByWorkload(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var decorated *endpointReservationFailureClient
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		ServiceController|ExecutableController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{ConfigurePortAllocator: true, DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			decorated = &endpointReservationFailureClient{Client: apiClient}
			return decorated
		}})
	require.NoError(t, startupErr)
	t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
	t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26120")
	db := reservationDatabase(t, environment)
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "producer"},
		Spec: apiv1.ServiceSpec{Protocol: commonapi.PortProtocolTCP, AddressAllocationMode: apiv1.AddressAllocationModeProxyless}}
	executable := &apiv1.Executable{ObjectMeta: metav1.ObjectMeta{Name: "producer",
		Annotations: map[string]string{commonapi.ServiceProducerAnnotation: `[{"serviceName":"producer","address":"127.0.0.1","port":26102}]`}},
		Spec: apiv1.ExecutableSpec{ExecutablePath: "port-reservation-producer"}}
	require.NoError(t, server.Client.Create(ctx, executable))
	waitObjectAssumesStateEx(t, ctx, server.Client, executable.NamespacedName(), func(current *apiv1.Executable) (bool, error) {
		return current.Status.State == apiv1.ExecutableStateRunning, nil
	})
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(ctx context.Context) (bool, error) {
		var endpoints apiv1.EndpointList
		listErr := server.Client.List(ctx, &endpoints)
		return len(endpoints.Items) == 1, listErr
	}))
	waitPortReservationCount(t, ctx, db, 0)
	require.NoError(t, server.Client.Create(ctx, service))
	waitObjectAssumesStateEx(t, ctx, server.Client, service.NamespacedName(), func(current *apiv1.Service) (bool, error) {
		return current.Status.State == apiv1.ServiceStateReady, nil
	})
	waitPortReservationCount(t, ctx, db, 1)
	require.NoError(t, server.Client.Delete(ctx, service))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, service)
	waitPortReservationCount(t, ctx, db, 1)
	require.NoError(t, server.Client.Delete(ctx, executable))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, executable)
	waitPortReservationCount(t, ctx, db, 0)

	decorated.fail.Store(true)
	failedService := service.DeepCopy()
	failedService.ObjectMeta = metav1.ObjectMeta{Name: "publication-failure"}
	require.NoError(t, server.Client.Create(ctx, failedService))
	failedExecutable := executable.DeepCopy()
	failedExecutable.ObjectMeta = metav1.ObjectMeta{Name: "publication-failure",
		Annotations: map[string]string{commonapi.ServiceProducerAnnotation: `[{"serviceName":"publication-failure","address":"127.0.0.1","port":26102}]`}}
	require.NoError(t, server.Client.Create(ctx, failedExecutable))
	waitObjectAssumesStateEx(t, ctx, server.Client, failedExecutable.NamespacedName(), func(current *apiv1.Executable) (bool, error) {
		return current.Status.State == apiv1.ExecutableStateRunning && decorated.failures.Load() > 0, nil
	})
	waitPortReservationCount(t, ctx, db, 1)
	decorated.fail.Store(false)
	waitObjectAssumesStateEx(t, ctx, server.Client, failedService.NamespacedName(), func(current *apiv1.Service) (bool, error) {
		return current.Status.State == apiv1.ServiceStateReady, nil
	})
	waitPortReservationCount(t, ctx, db, 1)
	require.NoError(t, server.Client.Delete(ctx, failedExecutable))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, failedExecutable)
	waitPortReservationCount(t, ctx, db, 0)
}

type delayedProducerContainerStop struct {
	containers.ContainerOrchestrator
	started chan struct{}
	resume  <-chan struct{}
}

func (orchestrator *delayedProducerContainerStop) StopContainers(
	ctx context.Context, options containers.StopContainersOptions,
) ([]string, error) {
	select {
	case orchestrator.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-orchestrator.resume:
		return orchestrator.ContainerOrchestrator.StopContainers(ctx, options)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Verifies that withdrawing Endpoints during asynchronous workload stopping does not release
// the producer's reservation until the process or container has actually stopped.
func TestProducerPortReservationRetainedUntilStopCompletes(t *testing.T) {
	for _, kind := range []string{"executable", "container"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			stopGate, completeStop := context.WithCancel(ctx)
			defer completeStop()
			started := make(chan struct{}, 1)
			options := TestEnvironmentOptions{ConfigurePortAllocator: true}
			if kind == "container" {
				options.DecorateContainerOrchestrator = func(orchestrator containers.ContainerOrchestrator, _ *statestore.Store) containers.ContainerOrchestrator {
					return &delayedProducerContainerStop{ContainerOrchestrator: orchestrator, started: started, resume: stopGate.Done()}
				}
			}
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
				ServiceController|ExecutableController|ContainerController, t.Name(), t.TempDir(), options)
			require.NoError(t, startupErr)
			t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
			t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26120")
			db := reservationDatabase(t, environment)
			service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "producer"},
				Spec: apiv1.ServiceSpec{Protocol: commonapi.PortProtocolTCP, AddressAllocationMode: apiv1.AddressAllocationModeProxyless}}
			require.NoError(t, server.Client.Create(ctx, service))
			metadata := metav1.ObjectMeta{Name: "producer", Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: `[{"serviceName":"producer","address":"127.0.0.1","port":26102}]`}}
			var workload ctrl_client.Object
			if kind == "executable" {
				const executablePath = "port-reservation-delayed-stop"
				environment.TestProcessExecutor.InstallAutoExecution(internal_testutil.AutoExecution{
					Condition: internal_testutil.ProcessSearchCriteria{Command: []string{executablePath}},
					RunCommand: func(execution *internal_testutil.ProcessExecution) int32 {
						select {
						case _, open := <-execution.Signal:
							if !open {
								return 0
							}
						case <-ctx.Done():
							return 0
						}
						select {
						case started <- struct{}{}:
						case <-ctx.Done():
							return 0
						}
						<-stopGate.Done()
						return 0
					},
				})
				metadata.Annotations[commonapi.ServiceProducerAnnotation] = `[{"serviceName":"producer","address":"127.0.0.1"}]`
				executable := &apiv1.Executable{ObjectMeta: metadata,
					Spec: apiv1.ExecutableSpec{ExecutablePath: executablePath, Args: []string{`{{portForServing "producer"}}`}}}
				require.NoError(t, server.Client.Create(ctx, executable))
				waitObjectAssumesStateEx(t, ctx, server.Client, executable.NamespacedName(), func(current *apiv1.Executable) (bool, error) {
					return current.Status.State == apiv1.ExecutableStateRunning, nil
				})
				workload = executable
			} else {
				container := &apiv1.Container{ObjectMeta: metadata, Spec: apiv1.ContainerSpec{
					Image: "producer-image",
					Ports: []apiv1.ContainerPort{{ContainerPort: 26102, HostPort: 26102, HostIP: "127.0.0.1"}},
				}}
				require.NoError(t, server.Client.Create(ctx, container))
				ensureContainerRunningEx(t, ctx, server.Client, server.ContainerOrchestrator, container)
				workload = container
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(ctx context.Context) (bool, error) {
				var endpoints apiv1.EndpointList
				listErr := server.Client.List(ctx, &endpoints)
				return len(endpoints.Items) == 1, listErr
			}))
			waitPortReservationCount(t, ctx, db, 1)
			require.NoError(t, server.Client.Delete(ctx, workload))
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("workload stop operation did not start")
			}
			require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(ctx context.Context) (bool, error) {
				var endpoints apiv1.EndpointList
				listErr := server.Client.List(ctx, &endpoints)
				return len(endpoints.Items) == 0, listErr
			}))
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(workload), workload))
			require.NotNil(t, workload.GetDeletionTimestamp())
			require.NotEmpty(t, workload.GetFinalizers())
			var heldReservations int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM port_allocations").Scan(&heldReservations))
			require.Equal(t, 1, heldReservations, "the stopping workload still owns its port")
			completeStop()
			switch typed := workload.(type) {
			case *apiv1.Executable:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, typed)
			case *apiv1.Container:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, typed)
			}
			waitPortReservationCount(t, ctx, db, 0)
		})
	}
}

// Verifies that LogicalProcess releases dynamically selected ports when
// subsequent argument template evaluation fails, before their API resources are deleted.
func TestLogicalProcessPortReservationsReleasedOnTemplateFailure(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{ConfigurePortAllocator: true})
	require.NoError(t, startupErr)
	t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
	t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26101")
	db := reservationDatabase(t, environment)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "reservations"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "producer", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
	require.NoError(t, server.Client.Create(ctx, service))
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "producer", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{
			Process: &apiv2.LogicalProcessConfig{ExecutablePath: "template-failure", Args: []string{`{{portFor ""}}`}},
			Ports: []apiv2.LogicalProcessPort{{LogicalServiceBinding: apiv2.LogicalServiceBinding{
				Name: "http", ServiceRef: service.Name}, Address: "127.0.0.1"}},
		}}
	require.NoError(t, server.Client.Create(ctx, resource))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseFailed, nil
	})
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalProcessReasonFailedToStart)
	waitPortReservationCount(t, ctx, db, 0)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that failed V2 port preparation releases earlier dynamic selections before retrying,
// and that deletion releases successful selections after the backing has settled.
func TestLogicalProcessPortReservationPreparationAndDeletion(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		LogicalProcessController|PhysicalProcessController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{ConfigurePortAllocator: true})
	require.NoError(t, startupErr)
	t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
	t.Setenv(networking.DCP_PORT_ALLOCATION_RANGE, "26100-26101")
	db := reservationDatabase(t, environment)
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "reservations"}}
	require.NoError(t, server.Client.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
	require.NoError(t, server.Client.Create(ctx, service))
	process := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "producer", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{Process: &apiv2.LogicalProcessConfig{ExecutablePath: "logical-port-reservation"},
			Ports: []apiv2.LogicalProcessPort{
				{LogicalServiceBinding: apiv2.LogicalServiceBinding{Name: "first", ServiceRef: service.Name}, Address: "127.0.0.1"},
				{LogicalServiceBinding: apiv2.LogicalServiceBinding{Name: "second", ServiceRef: "missing"}, Address: "127.0.0.1"},
			}}}
	require.NoError(t, server.Client.Create(ctx, process))
	waitObjectAssumesStateEx(t, ctx, server.Client, process.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhasePending && len(current.Status.Conditions) > 0, nil
	})
	waitPortReservationCount(t, ctx, db, 0)
	missing := service.DeepCopy()
	missing.ObjectMeta = metav1.ObjectMeta{Name: "missing", Namespace: namespace.Name}
	require.NoError(t, server.Client.Create(ctx, missing))
	waitObjectAssumesStateEx(t, ctx, server.Client, process.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
	})
	waitPortReservationCount(t, ctx, db, 2)
	require.NoError(t, server.Client.Delete(ctx, process))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, process)
	waitPortReservationCount(t, ctx, db, 0)
}
