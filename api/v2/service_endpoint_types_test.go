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
		{"unowned dual stack", "service", []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}}, nil},
		{"explicit namespace", "test-namespace/service", []ServiceAddress{{Address: "localhost", Port: 1}}, nil},
		{"highest port", "service", []ServiceAddress{{Address: "::1", Port: 65535}}, nil},
		{"same address different ports", "service", []ServiceAddress{{Address: "::1", Port: 5000}, {Address: "::1", Port: 5001}}, nil},
		{"missing reference", "", []ServiceAddress{{Address: "::1", Port: 5000}}, []string{"spec.serviceRef"}},
		{"cross namespace", "other/service", []ServiceAddress{{Address: "::1", Port: 5000}}, []string{"spec.serviceRef"}},
		{"invalid reference", "INVALID", []ServiceAddress{{Address: "::1", Port: 5000}}, []string{"spec.serviceRef"}},
		{"missing addresses", "service", nil, []string{"spec.addresses"}},
		{"empty addresses", "service", []ServiceAddress{}, []string{"spec.addresses"}},
		{"missing address", "service", []ServiceAddress{{Port: 5000}}, []string{"spec.addresses[0].address"}},
		{"blank address", "service", []ServiceAddress{{Address: " \t", Port: 5000}}, []string{"spec.addresses[0].address"}},
		{"unallocated port", "service", []ServiceAddress{{Address: "::1"}}, []string{"spec.addresses[0].port"}},
		{"negative port", "service", []ServiceAddress{{Address: "::1", Port: -1}}, []string{"spec.addresses[0].port"}},
		{"excessive port", "service", []ServiceAddress{{Address: "::1", Port: 65536}}, []string{"spec.addresses[0].port"}},
		{"duplicate address", "service", []ServiceAddress{{Address: "::1", Port: 5000}, {Address: "::1", Port: 5000}}, []string{"spec.addresses[1]"}},
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
			Addresses:  []ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}},
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
