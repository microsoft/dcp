/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that owner diagnostic forwarding uses registered resource behavior for every log-capable V2 kind.
func TestOwnerResourceLogUsesRegisteredType(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	directory := t.TempDir()
	log := testutil.NewLogWithResourceSinkForTesting(t.Name(), directory)
	for _, prototype := range apiv2.PersistentTypes {
		owner, supportsLogs := prototype.New().(interface {
			ctrl_client.Object
			GetResourceId() string
		})
		if !supportsLogs {
			continue
		}
		kinds, _, kindsErr := scheme.ObjectKinds(owner)
		require.NoError(t, kindsErr)
		require.Len(t, kinds, 1)
		owner.SetUID(types.UID("owner-" + strings.ToLower(kinds[0].Kind)))
		controller := true
		child := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{
			UID: types.UID("child-" + strings.ToLower(kinds[0].Kind)),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: kinds[0].GroupVersion().String(), Kind: kinds[0].Kind,
				Name: "owner", UID: owner.GetUID(), Controller: &controller}},
		}}
		scoped := log.WithValues(logger.RESOURCE_LOG_STREAM_ID, child.GetResourceId())
		withOwnerResourceLog(scoped, child, scheme).Error(errors.New("test diagnostic"), "Forwarded owner diagnostic")
		logger.ReleaseResourceLog(owner.GetResourceId())
		logger.ReleaseResourceLog(child.GetResourceId())
		file, openErr := usvc_io.OpenFileReadOnly(filepath.Join(directory, "resource-"+owner.GetResourceId()+".log"))
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		require.NoError(t, file.Close())
		require.NoError(t, readErr)
		require.Contains(t, string(contents), "Forwarded owner diagnostic")
	}
}

// Verifies that prepareLogicalPorts allocates a fresh available dynamic port after a failed
// preparation attempt even if its earlier choice becomes busy, while preserving explicit ports.
func TestPrepareLogicalPortsReevaluatesDynamicPortsOnRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	firstService := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "namespace"}}
	apiClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(firstService).Build()
	owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: "namespace"}}
	requested := []logicalPortRequest{
		{binding: apiv2.LogicalServiceBinding{Name: "dynamic", ServiceRef: firstService.Name}, address: "127.0.0.1"},
		{binding: apiv2.LogicalServiceBinding{Name: "explicit", ServiceRef: "second"}, address: "127.0.0.1", port: 8080},
	}
	partial, preparationErr := prepareLogicalPorts(ctx, apiClient, apiClient, owner, requested, logr.Discard())
	require.True(t, apierrors.IsNotFound(preparationErr))
	require.Len(t, partial, 1)
	require.Positive(t, partial[0].Port)
	var listenConfig net.ListenConfig
	occupied, listenErr := listenConfig.Listen(ctx, "tcp4", fmt.Sprintf("127.0.0.1:%d", partial[0].Port))
	require.NoError(t, listenErr)
	defer func() { require.NoError(t, occupied.Close()) }()
	secondService := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: owner.Namespace}}
	require.NoError(t, apiClient.Create(ctx, secondService))
	prepared, retryErr := prepareLogicalPorts(ctx, apiClient, apiClient, owner, requested, logr.Discard())
	require.NoError(t, retryErr)
	require.Len(t, prepared, 2)
	require.Positive(t, prepared[0].Port)
	require.NotEqual(t, partial[0].Port, prepared[0].Port)
	require.EqualValues(t, 8080, prepared[1].Port)
	for _, request := range requested {
		owner.Spec.Ports = append(owner.Spec.Ports, apiv2.LogicalProcessPort{
			LogicalServiceBinding: request.binding, Address: request.address, Port: request.port,
		})
	}
	data := &logicalProcessData{ports: logicalPortSelections(prepared, requested)}
	require.Equal(t, prepared[0].Port, data.ports[0].allocatedPort)
	require.Zero(t, data.ports[1].allocatedPort)
	require.Equal(t, prepared, data.portData(owner))
}

type portServiceCreateClient struct {
	ctrl_client.Client
	createErr error
	commit    bool
	mismatch  bool
	reads     int
	creates   int
}

func (apiClient *portServiceCreateClient) Get(ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption) error {
	apiClient.reads++
	return apiClient.Client.Get(ctx, key, object, options...)
}

func (apiClient *portServiceCreateClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	apiClient.creates++
	if apiClient.commit {
		service := object.(*apiv2.LogicalService).DeepCopy()
		service.OwnerReferences = []metav1.OwnerReference{{APIVersion: apiv2.GroupVersion.String(), Kind: "LogicalProcess", Name: "another-owner", UID: "another-uid"}}
		if apiClient.mismatch {
			service.Spec.Protocol = commonapi.PortProtocolUDP
		}
		if commitErr := apiClient.Client.Create(ctx, service, options...); commitErr != nil {
			return commitErr
		}
	}
	return apiClient.createErr
}

// Verifies that prepareLogicalPorts returns ordinary Create errors without confirmation, recovers
// a committed service on retry, and confirms AlreadyExists races through template compatibility.
func TestPrepareLogicalPortsClassifiesCreateOutcomes(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		commit    bool
		mismatch  bool
		exists    bool
		readCount int
	}{
		{name: "write-failure", readCount: 1},
		{name: "lost-response", commit: true, readCount: 1},
		{name: "matching-race", commit: true, exists: true, readCount: 2},
		{name: "incompatible-race", commit: true, mismatch: true, exists: true, readCount: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			scheme := runtime.NewScheme()
			require.NoError(t, apiv2.AddToScheme(scheme))
			createErr := errors.New("service Create response unavailable")
			var injectedErr error = createErr
			if testCase.exists {
				injectedErr = apierrors.NewAlreadyExists(apiv2.GroupVersion.WithResource("logicalservices").GroupResource(), "workload-management")
			}
			apiClient := &portServiceCreateClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
				createErr: injectedErr, commit: testCase.commit, mismatch: testCase.mismatch}
			owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "namespace"}}
			requested := []logicalPortRequest{{binding: apiv2.LogicalServiceBinding{Name: "http",
				Service: &apiv2.LogicalServiceTemplate{Name: "management"}}, port: 8080}}
			ports, prepareErr := prepareLogicalPorts(ctx, apiClient, apiClient, owner, requested, logr.Discard())
			require.Equal(t, testCase.readCount, apiClient.reads)
			require.Equal(t, 1, apiClient.creates)
			switch {
			case testCase.mismatch:
				require.ErrorIs(t, prepareErr, errLogicalServiceDependencyFailed)
				require.Empty(t, ports)
			case testCase.exists:
				require.NoError(t, prepareErr)
				require.Len(t, ports, 1)
			default:
				require.ErrorIs(t, prepareErr, createErr)
				require.False(t, apierrors.IsNotFound(prepareErr))
				require.Empty(t, ports)
			}
			if testCase.commit && !testCase.exists {
				recovered, retryErr := prepareLogicalPorts(ctx, apiClient, apiClient, owner, requested, logr.Discard())
				require.NoError(t, retryErr)
				require.Len(t, recovered, 1)
				require.Equal(t, 1, apiClient.creates)
				require.Equal(t, 2, apiClient.reads)
			}
		})
	}
}

// Verifies that prepareLogicalPorts rejects terminating and failed service dependencies before
// allocating a port, including failed services that still advertise an address.
func TestPrepareLogicalPortsRejectsTerminalServices(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	for _, terminating := range []bool{false, true} {
		t.Run(fmt.Sprint(terminating), func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, apiv2.AddToScheme(scheme))
			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "namespace"},
				Status: apiv2.LogicalServiceStatus{Phase: apiv2.LogicalServicePhaseFailed,
					EffectiveAddresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 8080}}}}
			if terminating {
				service.Status.Phase = apiv2.LogicalServicePhasePending
				service.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				service.Finalizers = []string{"tests.dcp.microsoft.com/hold"}
			}
			apiClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
			owner := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: service.Namespace}}
			ports, prepareErr := prepareLogicalPorts(ctx, apiClient, apiClient, owner,
				[]logicalPortRequest{{binding: apiv2.LogicalServiceBinding{Name: "http", ServiceRef: service.Name}}}, logr.Discard())
			require.ErrorIs(t, prepareErr, errLogicalServiceDependencyFailed)
			require.Empty(t, ports)
		})
	}
}

// Verifies that logical endpoint derivation normalizes wildcard and IPv6 addresses, matches
// container ports and protocols, excludes unpublished mappings, and sorts and deduplicates results.
func TestLogicalEndpointAddressDerivation(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		address  string
		expected string
	}{
		{networking.IPv4LocalhostDefaultAddress, networking.IPv4LocalhostDefaultAddress},
		{networking.IPv4AllInterfaceAddress, networking.IPv4LocalhostDefaultAddress},
		{networking.IPv6LocalhostDefaultAddress, networking.IPv6LocalhostDefaultAddress},
		{networking.IPv6AllInterfaceAddress, networking.IPv6LocalhostDefaultAddress},
		{"[2001:db8::1]", "[2001:db8::1]"},
		{networking.Localhost, networking.Localhost},
	} {
		t.Run(testCase.address, func(t *testing.T) {
			t.Parallel()
			port := logicalPortData{protocol: commonapi.PortProtocolTCP}
			port.Address, port.Port = testCase.address, 8080
			require.Equal(t, [][]apiv2.ServiceAddress{{{Address: testCase.expected, Port: 8080}}},
				processEndpointAddresses([]logicalPortData{port}))
			mappings := []apiv2.PhysicalContainerPortMapping{{
				ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: testCase.address, HostPort: 18080,
			}}
			if strings.HasPrefix(testCase.address, "[") {
				raw := mappings[0]
				raw.HostIP = networking.ToStandaloneAddress(testCase.address)
				mappings = append(mappings, raw)
			}
			require.Equal(t, [][]apiv2.ServiceAddress{{{Address: testCase.expected, Port: 18080}}},
				containerEndpointAddresses([]apiv2.LogicalContainerPort{{ContainerPort: 8080}}, []logicalPortData{port}, mappings))
		})
	}
	for _, testCase := range []struct {
		name     string
		mappings []apiv2.PhysicalContainerPortMapping
		expected []apiv2.ServiceAddress
	}{
		{name: "no mappings"},
		{name: "empty host address", mappings: []apiv2.PhysicalContainerPortMapping{{
			ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 18080,
		}}, expected: []apiv2.ServiceAddress{{Address: networking.IPv4LocalhostDefaultAddress, Port: 18080}}},
		{name: "different container port", mappings: []apiv2.PhysicalContainerPortMapping{{
			ContainerPort: 9090, Protocol: commonapi.PortProtocolTCP, HostPort: 8080,
		}}},
		{name: "different protocol", mappings: []apiv2.PhysicalContainerPortMapping{{
			ContainerPort: 8080, Protocol: commonapi.PortProtocolUDP, HostPort: 18080,
		}}},
		{name: "unpublished port", mappings: []apiv2.PhysicalContainerPortMapping{{
			ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv4LocalhostDefaultAddress,
		}}},
		{name: "invalid host ports", mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: -1},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 65536},
		}},
		{name: "matching port and protocol", mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 9090, Protocol: commonapi.PortProtocolTCP, HostPort: 8080},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolUDP, HostPort: 18081},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv4LocalhostDefaultAddress, HostPort: 18080},
		}, expected: []apiv2.ServiceAddress{{Address: networking.IPv4LocalhostDefaultAddress, Port: 18080}}},
		{name: "sorted unique addresses", mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv6LocalhostDefaultAddress, HostPort: 18080},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: "127.0.0.2", HostPort: 18080},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv4LocalhostDefaultAddress, HostPort: 19000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv4LocalhostDefaultAddress, HostPort: 18080},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv4AllInterfaceAddress, HostPort: 18080},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: net.IPv6loopback.String(), HostPort: 18080},
		}, expected: []apiv2.ServiceAddress{
			{Address: networking.IPv4LocalhostDefaultAddress, Port: 18080},
			{Address: "127.0.0.2", Port: 18080},
			{Address: networking.IPv6LocalhostDefaultAddress, Port: 18080},
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			original := slices.Clone(testCase.mappings)
			port := logicalPortData{protocol: commonapi.PortProtocolTCP}
			port.Port = 8080
			require.Equal(t, [][]apiv2.ServiceAddress{testCase.expected},
				containerEndpointAddresses([]apiv2.LogicalContainerPort{{ContainerPort: 8080}}, []logicalPortData{port}, testCase.mappings))
			require.Equal(t, original, testCase.mappings)
		})
	}
}

// Verifies that containerEndpointAddresses maps explicit host ports directly and attributes
// remaining host ports numerically to dynamic declarations, preserving address groups and aliases independently of inspection order.
func TestContainerEndpointHostPortAttribution(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name      string
		ports     []apiv2.LogicalContainerPort
		protocols []commonapi.PortProtocol
		mappings  []apiv2.PhysicalContainerPortMapping
		expected  [][]apiv2.ServiceAddress
	}{
		{name: "explicit ports", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080, HostPort: 9001}, {ContainerPort: 8080, HostPort: 9000},
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 9000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv6LocalhostDefaultAddress, HostPort: 9000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 9001},
		}, expected: [][]apiv2.ServiceAddress{
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 9001}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 9000}, {Address: networking.IPv6LocalhostDefaultAddress, Port: 9000}},
		}},
		{name: "dynamic ports use numeric order and group addresses", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080}, {ContainerPort: 8080},
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 10000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv6LocalhostDefaultAddress, HostPort: 900},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 900},
		}, expected: [][]apiv2.ServiceAddress{
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 900}, {Address: networking.IPv6LocalhostDefaultAddress, Port: 900}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 10000}},
		}},
		{name: "dynamic ports exclude explicit assignments", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080}, {ContainerPort: 8080, HostPort: 900}, {ContainerPort: 8080},
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 10000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 900},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 1000},
		}, expected: [][]apiv2.ServiceAddress{
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 1000}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 900}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 10000}},
		}},
		{name: "explicit aliases share a binding", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080, HostPort: 9000}, {ContainerPort: 8080, HostPort: 9000},
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 9000},
		}, expected: [][]apiv2.ServiceAddress{
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 9000}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 9000}},
		}},
		{name: "attribution is scoped to container port and protocol", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080}, {ContainerPort: 8080}, {ContainerPort: 8080}, {ContainerPort: 9090},
		}, protocols: []commonapi.PortProtocol{
			commonapi.PortProtocolTCP, commonapi.PortProtocolUDP, commonapi.PortProtocolTCP, commonapi.PortProtocolTCP,
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 10000},
			{ContainerPort: 9090, Protocol: commonapi.PortProtocolTCP, HostPort: 2000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolUDP, HostPort: 900},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 9000},
		}, expected: [][]apiv2.ServiceAddress{
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 9000}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 900}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 10000}},
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 2000}},
		}},
		{name: "missing explicit binding does not fall back", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080, HostPort: 9000},
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 9001},
		}, expected: [][]apiv2.ServiceAddress{nil}},
		{name: "missing dynamic binding is not assigned another address of the same port", ports: []apiv2.LogicalContainerPort{
			{ContainerPort: 8080}, {ContainerPort: 8080},
		}, mappings: []apiv2.PhysicalContainerPortMapping{
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostIP: networking.IPv6LocalhostDefaultAddress, HostPort: 9000},
			{ContainerPort: 8080, Protocol: commonapi.PortProtocolTCP, HostPort: 9000},
		}, expected: [][]apiv2.ServiceAddress{
			{{Address: networking.IPv4LocalhostDefaultAddress, Port: 9000}, {Address: networking.IPv6LocalhostDefaultAddress, Port: 9000}},
			nil,
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			originalPorts, originalMappings := slices.Clone(testCase.ports), slices.Clone(testCase.mappings)
			resolved := make([]logicalPortData, len(testCase.ports))
			for portIndex, port := range testCase.ports {
				resolved[portIndex].Port, resolved[portIndex].protocol = port.ContainerPort, commonapi.PortProtocolTCP
				if len(testCase.protocols) > 0 {
					resolved[portIndex].protocol = testCase.protocols[portIndex]
				}
			}
			require.Equal(t, testCase.expected, containerEndpointAddresses(testCase.ports, resolved, testCase.mappings))
			require.Equal(t, originalPorts, testCase.ports)
			require.Equal(t, originalMappings, testCase.mappings)
			reversed := slices.Clone(testCase.mappings)
			slices.Reverse(reversed)
			require.Equal(t, testCase.expected, containerEndpointAddresses(testCase.ports, resolved, reversed))
		})
	}
}

// Verifies that physicalContainerConfigFromLogicalSpec maps evaluated configuration and resolved
// ports without aliasing the logical spec or treating logical network references as physical references.
func TestPhysicalContainerConfigFromLogicalSpec(t *testing.T) {
	t.Parallel()
	for _, mode := range []apiv2.LogicalContainerMode{apiv2.LogicalContainerModeSession, apiv2.LogicalContainerModePersistent} {
		t.Run(string(mode), func(t *testing.T) {
			monitorPID := int64(42)
			spec := &apiv2.LogicalContainerSpec{Mode: mode, ContainerName: "container", MonitorPID: &monitorPID,
				MonitorTimestamp: metav1.NowMicro(), Container: &apiv2.LogicalContainerConfig{
					ImageRef: "image", Entrypoint: "entrypoint", Command: []string{"evaluated"},
					Env:          []commonapi.EnvVar{{Name: "VALUE", Value: "evaluated"}},
					VolumeMounts: []apiv2.VolumeMount{{Type: apiv2.NamedVolumeMount, VolumeRef: "volume", Target: "/data"}},
					Networks:     []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "logical-network"}},
					Labels:       []commonapi.Label{{Key: "app", Value: "value"}},
				}}
			original := spec.DeepCopy()
			ports := []apiv2.ContainerPort{{ContainerPort: 8080, HostPort: 18080, Protocol: commonapi.PortProtocolTCP}}
			config := physicalContainerConfigFromLogicalSpec(spec, ports)
			require.Equal(t, mode == apiv2.LogicalContainerModePersistent, config.RetainRuntimeContainer)
			require.Equal(t, spec.ContainerName, config.ContainerName)
			require.Equal(t, spec.Container.ImageRef, config.ImageRef)
			require.Equal(t, spec.Container.Entrypoint, config.Entrypoint)
			require.Equal(t, spec.Container.Command, config.Command)
			require.Equal(t, spec.Container.Env, config.Env)
			require.Equal(t, spec.Container.VolumeMounts, config.VolumeMounts)
			require.Equal(t, spec.Container.Labels, config.Labels)
			require.Equal(t, ports, config.Ports)
			require.Equal(t, spec.MonitorPID, config.MonitorPID)
			require.Equal(t, spec.MonitorTimestamp, config.MonitorTimestamp)
			require.Empty(t, config.Networks)
			config.Command[0], config.Env[0].Value = "changed", "changed"
			config.VolumeMounts[0].Target, config.Labels[0].Value = "/changed", "changed"
			*config.MonitorPID = 99
			config.Ports[0].HostPort = 28080
			require.Equal(t, original, spec)
			require.EqualValues(t, 18080, ports[0].HostPort)
		})
	}
}

// Verifies that physicalResourceCreationLabels overrides duplicate caller values for each
// DCP ownership label without modifying the input or unrelated user-label precedence.
func TestPhysicalResourceCreationLabelsOverrideCallerValues(t *testing.T) {
	t.Parallel()
	labels := []commonapi.Label{{Key: "app", Value: "first"}, {Key: "app", Value: "last"}}
	for _, key := range []string{PersistentLabel, uidLabel, CreatorProcessIdLabel, CreatorProcessStartTimeLabel} {
		labels = append(labels, commonapi.Label{Key: key, Value: "caller-first"}, commonapi.Label{Key: key, Value: "caller-last"})
	}
	original := slices.Clone(labels)
	thisProcess, processErr := process.This()
	require.NoError(t, processErr)
	actual := v2LabelsToMap(physicalResourceCreationLabels(labels, true, types.UID("resource-uid"), logr.Discard()))
	require.Equal(t, "true", actual[PersistentLabel])
	require.Equal(t, "resource-uid", actual[uidLabel])
	require.Equal(t, fmt.Sprintf("%d", thisProcess.Pid), actual[CreatorProcessIdLabel])
	require.Equal(t, thisProcess.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat), actual[CreatorProcessStartTimeLabel])
	require.Equal(t, "last", actual["app"])
	require.Equal(t, original, labels)
}

type namespaceReadFailureReader struct {
	ctrl_client.Reader
	readErr error
}

func (reader *namespaceReadFailureReader) Get(context.Context, ctrl_client.ObjectKey, ctrl_client.Object, ...ctrl_client.GetOption) error {
	return reader.readErr
}

// Verifies that checkNamespaceReadiness reports unavailable namespace reasons, preserves lookup
// errors, and returns ready only for initialized, active namespaces that are not being deleted.
func TestCheckNamespaceReadiness(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	for _, testCase := range []struct {
		name      string
		namespace *apiv2.Namespace
		reason    apiv2.ConditionReason
		readErr   error
	}{
		{name: "missing", reason: apiv2.PhysicalResourceReasonNamespaceNotFound},
		{name: "uninitialized", namespace: &apiv2.Namespace{}, reason: apiv2.PhysicalResourceReasonNamespaceNotReady},
		{name: "inactive", namespace: &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{namespaceFinalizer}}}, reason: apiv2.PhysicalResourceReasonNamespaceNotActive},
		{name: "terminating", namespace: &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{namespaceFinalizer}, DeletionTimestamp: &metav1.Time{Time: time.Now()}}}, reason: apiv2.PhysicalResourceReasonNamespaceTerminating},
		{name: "read failure", readErr: errors.New("namespace read failed"), reason: apiv2.PhysicalResourceReasonNamespaceLookupFailed},
		{name: "canceled lookup", readErr: context.Canceled, reason: apiv2.PhysicalResourceReasonNamespaceLookupFailed},
		{name: "active", namespace: &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{namespaceFinalizer}}, Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, apiv2.AddToScheme(scheme))
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if testCase.namespace != nil {
				testCase.namespace.Name = "namespace"
				builder = builder.WithObjects(testCase.namespace)
			}
			var reader ctrl_client.Reader = builder.Build()
			if testCase.readErr != nil {
				reader = &namespaceReadFailureReader{Reader: reader, readErr: testCase.readErr}
			}
			ready, reason, readinessErr := checkNamespaceReadiness(ctx, reader, "namespace")
			require.Equal(t, testCase.reason == "", ready)
			require.Equal(t, testCase.reason, reason)
			if testCase.readErr != nil {
				require.ErrorIs(t, readinessErr, testCase.readErr)
			} else {
				require.NoError(t, readinessErr)
			}
		})
	}
}

func TestMapNamespaceToReconcileRequests(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	services := []ctrl_client.Object{
		&apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "target"}},
		&apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "target"}},
		&apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other"}},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(services...).Build()
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	requestReconcile := reconciler.mapNamespaceToReconcileRequests(&apiv1.ServiceList{})
	requests := requestReconcile(context.Background(), &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target"}})

	names := make([]types.NamespacedName, len(requests))
	for i := range requests {
		names[i] = requests[i].NamespacedName
	}
	require.ElementsMatch(t, []types.NamespacedName{
		{Namespace: "target", Name: "first"},
		{Namespace: "target", Name: "second"},
	}, names)
}

func TestEnsureV1PhysicalResourcesNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	require.NoError(t, EnsureV1PhysicalResourcesNamespace(context.Background(), client))
	require.NoError(t, EnsureV1PhysicalResourcesNamespace(context.Background(), client))

	namespace := apiv2.Namespace{}
	getErr := client.Get(context.Background(), types.NamespacedName{Name: V1PhysicalResourcesNamespaceName}, &namespace)
	require.NoError(t, getErr)
	require.Equal(t, V1PhysicalResourcesNamespaceName, namespace.Name)
}

func TestEnsureV1PhysicalContainerNetworkReference(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	require.NoError(t, apiv2.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	network := &apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: "source-network",
			UID:  types.UID("source-network-uid"),
		},
		Status: apiv1.ContainerNetworkStatus{
			ID: "first-network-id",
		},
	}

	ensureErr := EnsureV1PhysicalContainerNetworkReference(context.Background(), client, network)
	require.NoError(t, ensureErr)

	resourceName := V1PhysicalContainerNetworkReferenceName(network.UID)
	physicalNetwork := apiv2.PhysicalContainerNetwork{}
	require.NoError(t, client.Get(context.Background(), resourceName, &physicalNetwork))
	require.Equal(t, network.Status.ID, physicalNetwork.Spec.NetworkID)
	require.Nil(t, physicalNetwork.Spec.Network)
	require.Equal(t, network.Name, physicalNetwork.Annotations[V1ContainerNetworkNameAnnotation])
}

// Verifies that callWithRetryAndVerification() can be stopped from retrying by returning a permanent error
func TestCallWithRetryAndVerificationPermanentError(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Second
	ctx, cancel := testutil.GetTestContext(t, timeout)
	defer cancel()

	attempt := 0
	start := time.Now()
	permanentError := errors.New("permanent error")
	temporaryError := errors.New("temporary error")

	_, err := callWithRetryAndVerification(
		ctx,
		exponentialBackoff(timeout),
		func(_ context.Context) error { return nil },
		func(_ context.Context) (string, error) {
			attempt++
			if attempt == 2 {
				return "", resiliency.Permanent(permanentError)
			}
			return "", temporaryError
		},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, permanentError)
	require.Equal(t, 2, attempt)
	require.WithinRangef(t, time.Now(), start, start.Add(timeout/2), "the call should have stopped after the second attmept, much sooner than the timeout")
}

func TestSaveChangesAcknowledgesStatusAfterSuccessfulSave(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-service"}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv1.Service{}).
		WithObjects(service).
		Build()
	currentService := &apiv1.Service{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: service.Name}, currentService))
	patch := ctrl_client.MergeFromWithOptions(currentService.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	currentService.Status.State = apiv1.ServiceStateReady
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	acknowledged := false
	_, saveErr := reconciler.SaveChanges(context.Background(), currentService, patch, statusChanged, func() {
		persistedService := &apiv1.Service{}
		require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: service.Name}, persistedService))
		require.Equal(t, apiv1.ServiceStateReady, persistedService.Status.State)
		acknowledged = true
	}, logr.Discard())

	require.NoError(t, saveErr)
	require.True(t, acknowledged)
}

func TestSaveChangesAcknowledgesStatusWhenAlreadyDurable(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-service"}}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	patch := ctrl_client.MergeFromWithOptions(service.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	acknowledged := false
	_, saveErr := reconciler.SaveChanges(context.Background(), service, patch, additionalReconciliationNeeded, func() {
		acknowledged = true
	}, logr.Discard())

	require.NoError(t, saveErr)
	require.True(t, acknowledged)
}

func TestSaveChangesDoesNotAcknowledgeStatusAfterConflict(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-service"}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv1.Service{}).
		WithObjects(service).
		Build()
	currentService := &apiv1.Service{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: service.Name}, currentService))
	patch := ctrl_client.MergeFromWithOptions(currentService.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	concurrentService := currentService.DeepCopy()
	concurrentService.Labels = map[string]string{"updated": "true"}
	require.NoError(t, client.Update(context.Background(), concurrentService))
	currentService.Status.State = apiv1.ServiceStateReady
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	acknowledged := false
	result, saveErr := reconciler.SaveChanges(context.Background(), currentService, patch, statusChanged, func() {
		acknowledged = true
	}, logr.Discard())

	require.NoError(t, saveErr)
	require.Positive(t, result.RequeueAfter)
	require.False(t, acknowledged)
}
