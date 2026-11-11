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

// Verifies that LogicalService.Validate accepts listener and proxyless configurations
// while rejecting invalid transports, ports, and allocation modes.
func TestLogicalServiceValidate(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name           string
		spec           LogicalServiceSpec
		expectedFields []string
	}{
		{"default allocation", LogicalServiceSpec{}, nil},
		{"TCP", LogicalServiceSpec{Protocol: commonapi.PortProtocolTCP, Port: 65535}, nil},
		{"UDP", LogicalServiceSpec{Protocol: commonapi.PortProtocolUDP, Port: 1}, nil},
		{"hostname", LogicalServiceSpec{Address: "localhost", Port: 5000}, nil},
		{"invalid protocol", LogicalServiceSpec{Protocol: "HTTP"}, []string{"spec.protocol"}},
		{"negative requested port", LogicalServiceSpec{Port: -1}, []string{"spec.port"}},
		{"excessive requested port", LogicalServiceSpec{Port: 65536}, []string{"spec.port"}},
		{"unknown allocation mode", LogicalServiceSpec{AddressAllocationMode: "unknown"}, []string{"spec.addressAllocationMode"}},
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
				Spec:       LogicalServiceSpec{AddressAllocationMode: mode},
			}
			require.Empty(t, service.Validate(context.Background()))
		})
	}
}

// Verifies that LogicalServiceSpec defaults to TCP without replacing explicitly supplied values.
func TestLogicalServiceSpecDefaults(t *testing.T) {
	t.Parallel()
	require.Equal(t, commonapi.PortProtocolTCP, (LogicalServiceSpec{}).EffectiveProtocol())
	require.Equal(t, commonapi.PortProtocolUDP, (LogicalServiceSpec{Protocol: commonapi.PortProtocolUDP}).EffectiveProtocol())
	require.Equal(t, commonapi.PortProtocol("invalid"), (LogicalServiceSpec{Protocol: "invalid"}).EffectiveProtocol())
}

// Verifies that LogicalServiceSpec.Equal accepts explicit defaults, compares every requested
// configuration field symmetrically, and leaves its inputs unchanged.
func TestLogicalServiceSpecEqual(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		left  LogicalServiceSpec
		right LogicalServiceSpec
		equal bool
	}{
		{"empty", LogicalServiceSpec{}, LogicalServiceSpec{}, true},
		{"protocol default", LogicalServiceSpec{}, LogicalServiceSpec{Protocol: commonapi.PortProtocolTCP}, true},
		{"allocation default", LogicalServiceSpec{}, LogicalServiceSpec{AddressAllocationMode: ServiceAddressAllocationModeLocalhost}, true},
		{"both defaults", LogicalServiceSpec{Address: "localhost", Port: 5000},
			LogicalServiceSpec{Protocol: commonapi.PortProtocolTCP, Address: "localhost", Port: 5000, AddressAllocationMode: ServiceAddressAllocationModeLocalhost}, true},
		{"protocol mismatch", LogicalServiceSpec{}, LogicalServiceSpec{Protocol: commonapi.PortProtocolUDP}, false},
		{"address mismatch", LogicalServiceSpec{Address: "127.0.0.1"}, LogicalServiceSpec{Address: "[::1]"}, false},
		{"port mismatch", LogicalServiceSpec{Port: 5000}, LogicalServiceSpec{Port: 5001}, false},
		{"allocation mismatch", LogicalServiceSpec{}, LogicalServiceSpec{AddressAllocationMode: ServiceAddressAllocationModeProxyless}, false},
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
		Spec:       LogicalServiceSpec{Protocol: commonapi.PortProtocolTCP, Address: "localhost", Port: 5000, AddressAllocationMode: ServiceAddressAllocationModeLocalhost},
	}
	for _, testCase := range []struct {
		name    string
		change  func(*LogicalService)
		allowed bool
	}{
		{"unchanged", func(*LogicalService) {}, true},
		{"status", func(service *LogicalService) {
			service.Status.EffectiveAddresses = []ServiceAddress{{Address: "[::1]", Port: 5000}}
		}, true},
		{"metadata", func(service *LogicalService) { service.Annotations = map[string]string{"key": "value"} }, true},
		{"protocol", func(service *LogicalService) { service.Spec.Protocol = commonapi.PortProtocolUDP }, false},
		{"address", func(service *LogicalService) { service.Spec.Address = "127.0.0.1" }, false},
		{"port", func(service *LogicalService) { service.Spec.Port++ }, false},
		{"allocation mode", func(service *LogicalService) {
			service.Spec.AddressAllocationMode = ServiceAddressAllocationModeProxyless
		}, false},
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
				ServiceRef: "service", Addresses: []ServiceAddress{{Address: "[::1]", Port: 5000}},
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
		Spec: ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []ServiceAddress{{Address: "[::1]", Port: 5000}}}}
	require.Contains(t, service.Validate(context.Background()).ToAggregate().Error(), commonapi.ErrResourceCreationProhibited.Error())
	require.Contains(t, endpoint.Validate(context.Background()).ToAggregate().Error(), commonapi.ErrResourceCreationProhibited.Error())
	updated := service.DeepCopy()
	updated.Status.Phase = LogicalServicePhaseRunning
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
	addresses := []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "[::1]", Port: 5001}}
	service := &LogicalService{
		TypeMeta:   metav1.TypeMeta{Kind: "LogicalService", APIVersion: GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "test-namespace"},
		Spec:       LogicalServiceSpec{Address: "localhost", Port: 5000},
		Status: LogicalServiceStatus{
			Phase: LogicalServicePhaseRunning, EffectiveAddresses: addresses,
			Conditions: []metav1.Condition{{Type: string(ConditionReady), Status: metav1.ConditionTrue, Reason: string(LogicalServiceReasonReady)}},
		},
	}
	encoded, encodeErr := json.Marshal(service)
	require.NoError(t, encodeErr)
	var document map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &document))
	var statusDocument map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(document["status"], &statusDocument))
	var specDocument map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(document["spec"], &specDocument))
	require.NotContains(t, specDocument, "mode")
	require.NotContains(t, specDocument, "addresses")
	require.Contains(t, statusDocument, "effectiveAddresses")
	require.NotContains(t, statusDocument, "effectiveAddress")
	require.NotContains(t, statusDocument, "effectivePort")
	var roundTrip LogicalService
	require.NoError(t, json.Unmarshal(encoded, &roundTrip))
	require.Equal(t, service.Spec, roundTrip.Spec)
	require.Equal(t, addresses, roundTrip.Status.EffectiveAddresses)

	copied := service.DeepCopy()
	copied.Spec.Port++
	copied.Status.EffectiveAddresses[1].Address = "localhost"
	copied.Status.Conditions[0].Reason = "Changed"
	require.Equal(t, int32(5000), service.Spec.Port)
	require.Equal(t, "[::1]", service.Status.EffectiveAddresses[1].Address)
	require.Equal(t, string(LogicalServiceReasonReady), service.Status.Conditions[0].Reason)
	projected := &LogicalService{}
	service.Status.CopyTo(projected)
	projected.Status.EffectiveAddresses[0].Port++
	require.Equal(t, int32(5000), service.Status.EffectiveAddresses[0].Port)
}
