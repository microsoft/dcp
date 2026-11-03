/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that LogicalService.Validate accepts session and existing services while rejecting
// persistence, invalid transports, and configuration belonging to the other mode.
func TestLogicalServiceValidate(t *testing.T) {
	t.Parallel()
	addresses := []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}}
	for _, testCase := range []struct {
		name           string
		spec           LogicalServiceSpec
		expectedFields []string
	}{
		{"default session", LogicalServiceSpec{}, nil},
		{"session TCP", LogicalServiceSpec{Mode: LogicalServiceModeSession, LogicalServiceConfig: LogicalServiceConfig{Protocol: commonapi.PortProtocolTCP, Port: 65535}}, nil},
		{"session UDP", LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{Protocol: commonapi.PortProtocolUDP, Port: 1}}, nil},
		{"existing without workload", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: addresses}, nil},
		{"existing UDP hostname", LogicalServiceSpec{Mode: LogicalServiceModeExisting, LogicalServiceConfig: LogicalServiceConfig{Protocol: commonapi.PortProtocolUDP}, Addresses: []ServiceAddress{{Address: "localhost", Port: 53}}}, nil},
		{"session with existing addresses", LogicalServiceSpec{Addresses: addresses}, []string{"spec.addresses"}},
		{"missing existing addresses", LogicalServiceSpec{Mode: LogicalServiceModeExisting}, []string{"spec.addresses"}},
		{"existing with requested address", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: addresses, LogicalServiceConfig: LogicalServiceConfig{Address: "localhost"}}, []string{"spec.address"}},
		{"existing with requested port", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: addresses, LogicalServiceConfig: LogicalServiceConfig{Port: 5000}}, []string{"spec.port"}},
		{"existing with allocation mode", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: addresses, LogicalServiceConfig: LogicalServiceConfig{AddressAllocationMode: ServiceAddressAllocationModeLocalhost}}, []string{"spec.addressAllocationMode"}},
		{"persistent unsupported", LogicalServiceSpec{Mode: "persistent"}, []string{"spec.mode"}},
		{"unknown mode", LogicalServiceSpec{Mode: "unknown"}, []string{"spec.mode"}},
		{"invalid protocol", LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{Protocol: "HTTP"}}, []string{"spec.protocol"}},
		{"negative requested port", LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{Port: -1}}, []string{"spec.port"}},
		{"excessive requested port", LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{Port: 65536}}, []string{"spec.port"}},
		{"unknown allocation mode", LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{AddressAllocationMode: "unknown"}}, []string{"spec.addressAllocationMode"}},
		{"blank existing address", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: []ServiceAddress{{Address: " ", Port: 1}}}, []string{"spec.addresses[0].address"}},
		{"unallocated existing port", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: []ServiceAddress{{Address: "::1"}}}, []string{"spec.addresses[0].port"}},
		{"duplicate existing address", LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: []ServiceAddress{addresses[0], addresses[0]}}, []string{"spec.addresses[1]"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &LogicalService{
				ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
				Spec:       testCase.spec,
			}
			errorList := service.Validate(context.Background())
			actualFields := make([]string, len(errorList))
			for i, validationError := range errorList {
				actualFields[i] = validationError.Field
			}
			require.ElementsMatch(t, testCase.expectedFields, actualFields, "%v", errorList)
		})
	}
}

// Verifies that LogicalService supports each V1 address-allocation choice, including proxyless selection.
func TestLogicalServiceValidateAddressAllocationModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []ServiceAddressAllocationMode{
		"", ServiceAddressAllocationModeLocalhost, ServiceAddressAllocationModeIPv4ZeroOne,
		ServiceAddressAllocationModeIPv4Loopback, ServiceAddressAllocationModeIPv6ZeroOne,
		ServiceAddressAllocationModeAllInterfaces, ServiceAddressAllocationModeIPv4AllInterfaces,
		ServiceAddressAllocationModeIPv6AllInterfaces, ServiceAddressAllocationModeProxyless,
	} {
		t.Run(string(mode), func(t *testing.T) {
			service := &LogicalService{
				ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
				Spec:       LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{AddressAllocationMode: mode}},
			}
			require.Empty(t, service.Validate(context.Background()))
		})
	}
}

// Verifies that LogicalServiceSpec defaults to session and TCP without replacing explicitly supplied values.
func TestLogicalServiceSpecDefaults(t *testing.T) {
	t.Parallel()
	require.Equal(t, LogicalServiceModeSession, (LogicalServiceSpec{}).EffectiveMode())
	require.Equal(t, commonapi.PortProtocolTCP, (LogicalServiceConfig{}).EffectiveProtocol())
	require.Equal(t, LogicalServiceModeExisting, (LogicalServiceSpec{Mode: LogicalServiceModeExisting}).EffectiveMode())
	require.Equal(t, commonapi.PortProtocolUDP, (LogicalServiceConfig{Protocol: commonapi.PortProtocolUDP}).EffectiveProtocol())
	require.Equal(t, LogicalServiceMode("invalid"), (LogicalServiceSpec{Mode: "invalid"}).EffectiveMode())
	require.Equal(t, commonapi.PortProtocol("invalid"), (LogicalServiceConfig{Protocol: "invalid"}).EffectiveProtocol())
}

// Verifies that LogicalServiceConfig.Equal accepts explicit defaults, compares every requested
// configuration field symmetrically, and leaves its inputs unchanged.
func TestLogicalServiceConfigEqual(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		left  LogicalServiceConfig
		right LogicalServiceConfig
		equal bool
	}{
		{"empty", LogicalServiceConfig{}, LogicalServiceConfig{}, true},
		{"protocol default", LogicalServiceConfig{}, LogicalServiceConfig{Protocol: commonapi.PortProtocolTCP}, true},
		{"allocation default", LogicalServiceConfig{}, LogicalServiceConfig{AddressAllocationMode: ServiceAddressAllocationModeLocalhost}, true},
		{"both defaults", LogicalServiceConfig{Address: "localhost", Port: 5000},
			LogicalServiceConfig{Protocol: commonapi.PortProtocolTCP, Address: "localhost", Port: 5000, AddressAllocationMode: ServiceAddressAllocationModeLocalhost}, true},
		{"protocol mismatch", LogicalServiceConfig{}, LogicalServiceConfig{Protocol: commonapi.PortProtocolUDP}, false},
		{"address mismatch", LogicalServiceConfig{Address: "127.0.0.1"}, LogicalServiceConfig{Address: "::1"}, false},
		{"port mismatch", LogicalServiceConfig{Port: 5000}, LogicalServiceConfig{Port: 5001}, false},
		{"allocation mismatch", LogicalServiceConfig{}, LogicalServiceConfig{AddressAllocationMode: ServiceAddressAllocationModeProxyless}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			leftBefore, rightBefore := testCase.left, testCase.right
			require.Equal(t, testCase.equal, testCase.left.Equal(testCase.right))
			require.Equal(t, testCase.equal, testCase.right.Equal(testCase.left))
			require.Equal(t, leftBefore, testCase.left)
			require.Equal(t, rightBefore, testCase.right)
		})
	}
}

// Verifies that LogicalService.ValidateUpdate freezes every configuration field while allowing metadata and status changes.
func TestLogicalServiceValidateUpdate(t *testing.T) {
	t.Parallel()
	original := &LogicalService{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Spec: LogicalServiceSpec{
			Mode:                 LogicalServiceModeSession,
			LogicalServiceConfig: LogicalServiceConfig{Protocol: commonapi.PortProtocolTCP, Address: "localhost", Port: 5000, AddressAllocationMode: ServiceAddressAllocationModeLocalhost},
		},
	}
	for _, testCase := range []struct {
		name    string
		change  func(*LogicalService)
		allowed bool
	}{
		{"unchanged", func(*LogicalService) {}, true},
		{"status", func(service *LogicalService) {
			service.Status.EffectiveAddresses = []ServiceAddress{{Address: "::1", Port: 5000}}
		}, true},
		{"metadata", func(service *LogicalService) { service.Annotations = map[string]string{"key": "value"} }, true},
		{"mode", func(service *LogicalService) { service.Spec.Mode = LogicalServiceModeExisting }, false},
		{"protocol", func(service *LogicalService) { service.Spec.Protocol = commonapi.PortProtocolUDP }, false},
		{"address", func(service *LogicalService) { service.Spec.Address = "127.0.0.1" }, false},
		{"port", func(service *LogicalService) { service.Spec.Port++ }, false},
		{"allocation mode", func(service *LogicalService) {
			service.Spec.AddressAllocationMode = ServiceAddressAllocationModeProxyless
		}, false},
		{"existing addresses", func(service *LogicalService) { service.Spec.Addresses = []ServiceAddress{{Address: "::1", Port: 5000}} }, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			updated := original.DeepCopy()
			testCase.change(updated)
			errorList := updated.ValidateUpdate(context.Background(), original)
			if testCase.allowed {
				require.Empty(t, errorList)
			} else {
				require.Len(t, errorList, 1)
				require.Equal(t, "spec", errorList[0].Field)
			}
		})
	}
}

// Verifies that LogicalService and ServiceEndpoint require valid namespaces and bounded annotations.
func TestServiceResourcesValidateMetadata(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name          string
		namespace     string
		annotations   map[string]string
		expectedField string
	}{
		{"missing namespace", "", nil, "metadata.namespace"},
		{"invalid namespace", "Invalid_Namespace", nil, "metadata.namespace"},
		{"oversized annotations", "test-namespace", map[string]string{"large": strings.Repeat("a", commonapi.MaxAnnotationsTotalSize)}, "metadata.annotations"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := metav1.ObjectMeta{Name: "service", Namespace: testCase.namespace, Annotations: testCase.annotations}
			service := &LogicalService{ObjectMeta: metadata}
			serviceErrors := service.Validate(context.Background())
			require.NotEmpty(t, serviceErrors)
			require.Equal(t, testCase.expectedField, serviceErrors[0].Field)
			endpoint := &ServiceEndpoint{ObjectMeta: metadata, Spec: ServiceEndpointSpec{
				ServiceRef: "service", Addresses: []ServiceAddress{{Address: "::1", Port: 5000}},
			}}
			endpointErrors := endpoint.Validate(context.Background())
			require.NotEmpty(t, endpointErrors)
			require.Equal(t, testCase.expectedField, endpointErrors[0].Field)
		})
	}
}

// Verifies that service resources prohibit creation during shutdown while permitting deletion and service status updates.
func TestServiceResourcesValidateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)
	service := &LogicalService{
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
	}
	endpoint := &ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test-namespace"},
		Spec: ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []ServiceAddress{{Address: "::1", Port: 5000}}}}
	require.Contains(t, service.Validate(context.Background()).ToAggregate().Error(), commonapi.ErrResourceCreationProhibited.Error())
	require.Contains(t, endpoint.Validate(context.Background()).ToAggregate().Error(), commonapi.ErrResourceCreationProhibited.Error())
	updated := service.DeepCopy()
	updated.Status.Phase = LogicalServicePhaseReady
	require.Empty(t, updated.ValidateUpdate(context.Background(), service))
	deletionTime := metav1.Now()
	service.DeletionTimestamp, endpoint.DeletionTimestamp = &deletionTime, &deletionTime
	require.Empty(t, service.Validate(context.Background()))
	require.Empty(t, endpoint.Validate(context.Background()))
}

// Verifies that LogicalService registers with namespaced storage and standard list/status interfaces.
func TestLogicalServiceRegistration(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	registered, registrationErr := scheme.New(GroupVersion.WithKind("LogicalService"))
	require.NoError(t, registrationErr)
	require.IsType(t, &LogicalService{}, registered)
	registeredList, listErr := scheme.New(GroupVersion.WithKind("LogicalServiceList"))
	require.NoError(t, listErr)
	require.IsType(t, &LogicalServiceList{}, registeredList)
	service := &LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"}}
	require.Contains(t, PersistentTypes, &LogicalService{})
	require.Equal(t, GroupVersion.WithResource("logicalservices"), service.GetGroupVersionResource())
	require.Equal(t, types.NamespacedName{Namespace: "test-namespace", Name: "service"}, service.NamespacedName())
	require.True(t, service.NamespaceScoped())
	require.True(t, service.IsStorageVersion())
	require.Equal(t, []string{"lsvc"}, service.ShortNames())
	require.IsType(t, &LogicalService{}, service.New())
	require.IsType(t, &LogicalServiceList{}, service.NewList())
	require.Same(t, &service.ObjectMeta, service.GetObjectMeta())
	require.IsType(t, LogicalServiceStatus{}, service.GetStatus())
	list := &LogicalServiceList{Items: []LogicalService{*service}}
	require.Same(t, &list.ListMeta, list.GetListMeta())
	require.Equal(t, uint32(1), list.ItemCount())
	require.Same(t, &list.Items[0], list.GetItems()[0])
}

// Verifies that LogicalService JSON retains multiple effective address/port pairs and deep copies isolate configuration and status.
func TestLogicalServiceJSONAndDeepCopy(t *testing.T) {
	t.Parallel()
	addresses := []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}}
	service := &LogicalService{
		TypeMeta:   metav1.TypeMeta{Kind: "LogicalService", APIVersion: GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Spec:       LogicalServiceSpec{Mode: LogicalServiceModeExisting, Addresses: addresses},
		Status: LogicalServiceStatus{
			Phase: LogicalServicePhaseReady, EffectiveAddresses: addresses,
			Conditions: []metav1.Condition{{Type: string(ConditionReady), Status: metav1.ConditionTrue, Reason: "ExistingService"}},
		},
	}
	encoded, encodeErr := json.Marshal(service)
	require.NoError(t, encodeErr)
	var document map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &document))
	var statusDocument map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(document["status"], &statusDocument))
	require.Contains(t, statusDocument, "effectiveAddresses")
	require.NotContains(t, statusDocument, "effectiveAddress")
	require.NotContains(t, statusDocument, "effectivePort")
	var roundTrip LogicalService
	require.NoError(t, json.Unmarshal(encoded, &roundTrip))
	require.Equal(t, addresses, roundTrip.Spec.Addresses)
	require.Equal(t, addresses, roundTrip.Status.EffectiveAddresses)
	require.Empty(t, roundTrip.Spec.Address)

	copied := service.DeepCopy()
	copied.Spec.Addresses[0].Port++
	copied.Status.EffectiveAddresses[1].Address = "localhost"
	copied.Status.Conditions[0].Reason = "Changed"
	require.Equal(t, int32(5000), service.Spec.Addresses[0].Port)
	require.Equal(t, "::1", service.Status.EffectiveAddresses[1].Address)
	require.Equal(t, "ExistingService", service.Status.Conditions[0].Reason)
	projected := &LogicalService{}
	service.Status.CopyTo(projected)
	projected.Status.EffectiveAddresses[0].Port++
	require.Equal(t, int32(5000), service.Status.EffectiveAddresses[0].Port)

	session := &LogicalService{Spec: LogicalServiceSpec{LogicalServiceConfig: LogicalServiceConfig{Port: 5000}}}
	sessionCopy := session.DeepCopy()
	sessionCopy.Spec.Port++
	require.Equal(t, int32(5000), session.Spec.Port)
}
