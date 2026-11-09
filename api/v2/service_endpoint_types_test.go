/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// Verifies that ServiceEndpoint.Validate requires same-namespace service references and distinct, allocated address/port pairs.
func TestServiceEndpointValidate(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name           string
		serviceRef     string
		addresses      []ServiceAddress
		expectedFields []string
	}{
		{"unowned dual stack", "service", []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "[::1]", Port: 5001}}, nil},
		{"explicit namespace", "test-namespace/service", []ServiceAddress{{Address: "localhost", Port: 1}}, nil},
		{"highest port", "service", []ServiceAddress{{Address: "[::1]", Port: 65535}}, nil},
		{"same address different ports", "service", []ServiceAddress{{Address: "[::1]", Port: 5000}, {Address: "[::1]", Port: 5001}}, nil},
		{"missing reference", "", []ServiceAddress{{Address: "[::1]", Port: 5000}}, []string{"spec.serviceRef"}},
		{"cross namespace", "other/service", []ServiceAddress{{Address: "[::1]", Port: 5000}}, []string{"spec.serviceRef"}},
		{"invalid reference", "INVALID", []ServiceAddress{{Address: "[::1]", Port: 5000}}, []string{"spec.serviceRef"}},
		{"missing addresses", "service", nil, []string{"spec.addresses"}},
		{"empty addresses", "service", []ServiceAddress{}, []string{"spec.addresses"}},
		{"missing address", "service", []ServiceAddress{{Port: 5000}}, []string{"spec.addresses[0].address"}},
		{"blank address", "service", []ServiceAddress{{Address: " \t", Port: 5000}}, []string{"spec.addresses[0].address"}},
		{"unallocated port", "service", []ServiceAddress{{Address: "[::1]"}}, []string{"spec.addresses[0].port"}},
		{"negative port", "service", []ServiceAddress{{Address: "[::1]", Port: -1}}, []string{"spec.addresses[0].port"}},
		{"excessive port", "service", []ServiceAddress{{Address: "[::1]", Port: 65536}}, []string{"spec.addresses[0].port"}},
		{"duplicate address", "service", []ServiceAddress{{Address: "[::1]", Port: 5000}, {Address: "[::1]", Port: 5000}}, []string{"spec.addresses[1]"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint := &ServiceEndpoint{
				ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test-namespace"},
				Spec:       ServiceEndpointSpec{ServiceRef: testCase.serviceRef, Addresses: testCase.addresses},
			}
			errorList := endpoint.Validate(context.Background())
			actualFields := make([]string, len(errorList))
			for i, validationError := range errorList {
				actualFields[i] = validationError.Field
			}
			require.ElementsMatch(t, testCase.expectedFields, actualFields, "%v", errorList)
		})
	}
}

// Verifies that ServiceEndpoint metadata and finalizer updates remain valid during shutdown,
// while creation is prohibited and configuration changes remain forbidden.
func TestServiceEndpointValidateUpdateDuringShutdown(t *testing.T) {
	commonapi.ResourceCreationProhibited.Store(true)
	defer commonapi.ResourceCreationProhibited.Store(false)
	original := &ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test-namespace", Finalizers: []string{"test/cleanup"}},
		Spec: ServiceEndpointSpec{ServiceRef: "service", Addresses: []ServiceAddress{{Address: "127.0.0.1", Port: 5000}}}}
	require.NotEmpty(t, original.Validate(context.Background()))
	updated := original.DeepCopy()
	updated.Annotations = map[string]string{"test": "updated"}
	require.Empty(t, updated.ValidateUpdate(context.Background(), original))
	deletionTime := metav1.Now()
	updated.DeletionTimestamp = &deletionTime
	updated.Finalizers = nil
	require.Empty(t, updated.ValidateUpdate(context.Background(), original))
	updated.Spec.Addresses[0].Port = 0
	require.NotEmpty(t, updated.ValidateUpdate(context.Background(), original))
}

// Verifies that ServiceEndpoint.ValidateUpdate rejects changes to either spec field, including
// otherwise valid routing changes, while allowing metadata updates and an unchanged spec.
func TestServiceEndpointValidateUpdate(t *testing.T) {
	t.Parallel()
	original := &ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test-namespace"},
		Spec: ServiceEndpointSpec{ServiceRef: "service", Addresses: []ServiceAddress{{Address: "127.0.0.1", Port: 5000}}}}
	for _, testCase := range []struct {
		name   string
		change func(*ServiceEndpoint)
	}{
		{"service", func(endpoint *ServiceEndpoint) { endpoint.Spec.ServiceRef = "other" }},
		{"address", func(endpoint *ServiceEndpoint) { endpoint.Spec.Addresses[0].Address = "[::1]" }},
		{"port", func(endpoint *ServiceEndpoint) { endpoint.Spec.Addresses[0].Port++ }},
		{"add address", func(endpoint *ServiceEndpoint) {
			endpoint.Spec.Addresses = append(endpoint.Spec.Addresses, ServiceAddress{Address: "[::1]", Port: 5000})
		}},
		{"remove addresses", func(endpoint *ServiceEndpoint) { endpoint.Spec.Addresses = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			updated := original.DeepCopy()
			testCase.change(updated)
			updateErrors := updated.ValidateUpdate(context.Background(), original)
			require.Len(t, updateErrors, 1)
			require.Equal(t, "spec", updateErrors[0].Field)
			require.Contains(t, updateErrors[0].Error(), "spec is immutable")
		})
	}
	updated := original.DeepCopy()
	updated.Annotations = map[string]string{"test": "updated"}
	updated.Finalizers = []string{"test/cleanup"}
	require.Empty(t, updated.ValidateUpdate(context.Background(), original))
}

// Verifies that ServiceEndpoint registers with namespaced storage and standard list interfaces.
func TestServiceEndpointRegistration(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	registered, registrationErr := scheme.New(GroupVersion.WithKind("ServiceEndpoint"))
	require.NoError(t, registrationErr)
	require.IsType(t, &ServiceEndpoint{}, registered)
	registeredList, listErr := scheme.New(GroupVersion.WithKind("ServiceEndpointList"))
	require.NoError(t, listErr)
	require.IsType(t, &ServiceEndpointList{}, registeredList)
	endpoint := &ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test-namespace"}}
	require.Contains(t, PersistentTypes, &ServiceEndpoint{})
	require.Equal(t, GroupVersion.WithResource("serviceendpoints"), endpoint.GetGroupVersionResource())
	require.Equal(t, types.NamespacedName{Namespace: "test-namespace", Name: "endpoint"}, endpoint.NamespacedName())
	require.True(t, endpoint.NamespaceScoped())
	require.True(t, endpoint.IsStorageVersion())
	require.Equal(t, []string{"sep"}, endpoint.ShortNames())
	require.IsType(t, &ServiceEndpoint{}, endpoint.New())
	require.IsType(t, &ServiceEndpointList{}, endpoint.NewList())
	require.Same(t, &endpoint.ObjectMeta, endpoint.GetObjectMeta())
	list := &ServiceEndpointList{Items: []ServiceEndpoint{*endpoint}}
	require.Same(t, &list.ListMeta, list.GetListMeta())
	require.Equal(t, uint32(1), list.ItemCount())
	require.Same(t, &list.Items[0], list.GetItems()[0])
}

// Verifies that ServiceEndpoint JSON retains alternative addresses without requiring an owner and deep copies isolate routing records.
func TestServiceEndpointJSONAndDeepCopy(t *testing.T) {
	t.Parallel()
	endpoint := &ServiceEndpoint{
		TypeMeta:   metav1.TypeMeta{Kind: "ServiceEndpoint", APIVersion: GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "test-namespace"},
		Spec: ServiceEndpointSpec{
			ServiceRef: "service",
			Addresses:  []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "[::1]", Port: 5001}},
		},
	}
	encoded, encodeErr := json.Marshal(endpoint)
	require.NoError(t, encodeErr)
	var roundTrip ServiceEndpoint
	require.NoError(t, json.Unmarshal(encoded, &roundTrip))
	require.Equal(t, endpoint.Spec, roundTrip.Spec)
	require.Empty(t, roundTrip.OwnerReferences)
	copied := endpoint.DeepCopy()
	copied.Spec.Addresses[0].Port++
	require.Equal(t, int32(5000), endpoint.Spec.Addresses[0].Port)
}
