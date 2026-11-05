/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

func recreateServiceEndpoint(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, desired *apiv2.ServiceEndpoint) *apiv2.ServiceEndpoint {
	t.Helper()
	previousUID := desired.UID
	require.NoError(t, apiClient.Delete(ctx, desired))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, desired)
	recreated := &apiv2.ServiceEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace, Annotations: desired.Annotations},
		Spec:       desired.DeepCopy().Spec,
	}
	require.NoError(t, apiClient.Create(ctx, recreated))
	require.NotEqual(t, previousUID, recreated.UID)
	return recreated
}

// Verifies that endpoint creation rejects invalid references and address pairs, updates and patches
// reject all spec changes, and routing changes require replacing endpoints with new identities.
func TestV2ServiceEndpointAPIValidatesUpdates(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startupErr := StartTestEnvironment(t, ctx, NamespaceController|LogicalServiceController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	apiClient := server.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-endpoint-update-validation"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
	require.NoError(t, apiClient.Create(ctx, service))
	endpoint := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: namespace.Name},
		Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}}}}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
	})
	for _, testCase := range []struct {
		name  string
		field string
		spec  apiv2.ServiceEndpointSpec
	}{
		{"missing-reference", "spec.serviceRef", apiv2.ServiceEndpointSpec{Addresses: endpoint.Spec.Addresses}},
		{"cross-namespace", "spec.serviceRef", apiv2.ServiceEndpointSpec{ServiceRef: "other/service", Addresses: endpoint.Spec.Addresses}},
		{"invalid-reference", "spec.serviceRef", apiv2.ServiceEndpointSpec{ServiceRef: "INVALID", Addresses: endpoint.Spec.Addresses}},
		{"empty-addresses", "spec.addresses", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{}}},
		{"missing-addresses", "spec.addresses", apiv2.ServiceEndpointSpec{ServiceRef: service.Name}},
		{"blank-address", "spec.addresses[0].address", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: " \t", Port: 5000}}}},
		{"zero-port", "spec.addresses[0].port", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1"}}}},
		{"negative-port", "spec.addresses[0].port", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: -1}}}},
		{"excessive-port", "spec.addresses[0].port", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 65536}}}},
		{"duplicate-address", "spec.addresses[1]", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{
			{Address: "127.0.0.1", Port: 5000}, {Address: "127.0.0.1", Port: 5000},
		}}},
		{"valid-new-service", "", apiv2.ServiceEndpointSpec{ServiceRef: "different-service", Addresses: endpoint.Spec.Addresses}},
		{"valid-new-address", "", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "::1", Port: 5000}}}},
		{"valid-new-port", "", apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5001}}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			invalid := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: namespace.Name}, Spec: testCase.spec}
			createErr := apiClient.Create(ctx, invalid)
			if testCase.field != "" {
				require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
				require.Contains(t, createErr.Error(), testCase.field)
			} else {
				require.NoError(t, createErr)
				require.NoError(t, apiClient.Delete(ctx, invalid))
			}
			for _, operation := range []string{"update", "patch"} {
				t.Run(operation, func(t *testing.T) {
					current := &apiv2.ServiceEndpoint{}
					require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), current))
					stored := current.DeepCopy()
					current.Spec = testCase.spec
					var updateErr error
					if operation == "update" {
						updateErr = apiClient.Update(ctx, current)
					} else {
						payload, marshalErr := json.Marshal(map[string]any{"spec": testCase.spec})
						require.NoError(t, marshalErr)
						updateErr = apiClient.Patch(ctx, current, ctrl_client.RawPatch(types.MergePatchType, payload))
					}
					require.True(t, apierrors.IsInvalid(updateErr), "%v", updateErr)
					require.Contains(t, updateErr.Error(), "spec is immutable")
					unchanged := &apiv2.ServiceEndpoint{}
					require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), unchanged))
					require.Equal(t, stored.Spec, unchanged.Spec)
					require.Equal(t, stored.ResourceVersion, unchanged.ResourceVersion)
				})
			}
		})
	}

	endpoint.Spec.ServiceRef = namespace.Name + "/" + service.Name
	endpoint.Spec.Addresses = []apiv2.ServiceAddress{{Address: "::1", Port: 5001}, {Address: "127.0.0.1", Port: 5002}}
	endpoint = recreateServiceEndpoint(t, ctx, apiClient, endpoint)
	waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseReady &&
			len(current.Status.EffectiveAddresses) == 2 && current.Status.EffectiveAddresses[0].Port == 5001, nil
	})
	require.NoError(t, apiClient.Patch(ctx, endpoint, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"test":"updated"}}}`))))
	endpoint.Spec.Addresses = []apiv2.ServiceAddress{{Address: "localhost", Port: 65535}}
	endpoint = recreateServiceEndpoint(t, ctx, apiClient, endpoint)
	waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseReady && len(current.Status.EffectiveAddresses) == 1 &&
			current.Status.EffectiveAddresses[0] == (apiv2.ServiceAddress{Address: "localhost", Port: 65535}), nil
	})

	// Generic metadata validation also protects immutable-spec resources during updates.
	serviceUpdate := &apiv2.LogicalService{}
	require.NoError(t, apiClient.Get(ctx, service.NamespacedName(), serviceUpdate))
	serviceUpdate.Annotations = map[string]string{"oversized": strings.Repeat("x", commonapi.MaxAnnotationsTotalSize)}
	metadataErr := apiClient.Update(ctx, serviceUpdate)
	require.True(t, apierrors.IsInvalid(metadataErr), "%v", metadataErr)
	require.Contains(t, metadataErr.Error(), "metadata.annotations")

	require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), endpoint))
	endpoint.Finalizers = []string{"tests.dcp.microsoft.com/hold"}
	require.NoError(t, apiClient.Update(ctx, endpoint))
	require.NoError(t, apiClient.Delete(ctx, endpoint))
	require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), endpoint))
	require.False(t, endpoint.DeletionTimestamp.IsZero())
	endpoint.Finalizers = nil
	require.NoError(t, apiClient.Update(ctx, endpoint))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, endpoint)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
}

// Verifies that LogicalService and ServiceEndpoint are discoverable alongside V1 kinds,
// support address/port pairs and status isolation, and are removed by namespace cleanup.
func TestV2ServiceAPIStorageAndNamespaceCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	serverInfo, _, startupErr := StartTestEnvironment(t, ctx, NamespaceController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	apiClient := serverInfo.Client
	for _, testCase := range []struct {
		version   schema.GroupVersion
		resources map[string]string
	}{
		{apiv1.GroupVersion, map[string]string{"services": "Service", "endpoints": "Endpoint"}},
		{apiv2.GroupVersion, map[string]string{"logicalservices": "LogicalService", "serviceendpoints": "ServiceEndpoint"}},
	} {
		discoveryBody, discoveryErr := serverInfo.RestClient.Get().AbsPath("/apis/" + testCase.version.String()).DoRaw(ctx)
		require.NoError(t, discoveryErr)
		var discovery metav1.APIResourceList
		require.NoError(t, json.Unmarshal(discoveryBody, &discovery))
		found := make(map[string]bool)
		for _, resource := range discovery.APIResources {
			if expectedKind, relevant := testCase.resources[resource.Name]; relevant {
				require.Equal(t, expectedKind, resource.Kind)
				require.Equal(t, testCase.version == apiv2.GroupVersion, resource.Namespaced)
				found[resource.Name] = true
			}
		}
		require.Len(t, found, len(testCase.resources))
	}

	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-service-api"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	allocated := &apiv2.LogicalService{
		ObjectMeta: metav1.ObjectMeta{Name: "allocated", Namespace: namespace.Name},
	}
	require.NoError(t, apiClient.Create(ctx, allocated))
	addresses := []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}}
	proxyless := &apiv2.LogicalService{
		ObjectMeta: metav1.ObjectMeta{Name: "proxyless", Namespace: namespace.Name},
		Spec: apiv2.LogicalServiceSpec{
			AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless,
		},
	}
	require.NoError(t, apiClient.Create(ctx, proxyless))
	require.Empty(t, proxyless.OwnerReferences)
	endpoint := &apiv2.ServiceEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: namespace.Name},
		Spec:       apiv2.ServiceEndpointSpec{ServiceRef: allocated.Name, Addresses: addresses},
	}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	require.Empty(t, endpoint.OwnerReferences)
	storedEndpoint := &apiv2.ServiceEndpoint{}
	require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), storedEndpoint))
	require.Equal(t, addresses, storedEndpoint.Spec.Addresses)
	storedEndpoint.Spec.Addresses[1].Port = 5002
	storedEndpoint = recreateServiceEndpoint(t, ctx, apiClient, storedEndpoint)
	require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), storedEndpoint))
	require.Equal(t, int32(5002), storedEndpoint.Spec.Addresses[1].Port)

	proxyless.Status = apiv2.LogicalServiceStatus{Phase: apiv2.LogicalServicePhaseReady, EffectiveAddresses: addresses}
	require.NoError(t, apiClient.Status().Update(ctx, proxyless))
	storedService := &apiv2.LogicalService{}
	require.NoError(t, apiClient.Get(ctx, proxyless.NamespacedName(), storedService))
	require.Equal(t, apiv2.ServiceAddressAllocationModeProxyless, storedService.Spec.AddressAllocationMode)
	require.Equal(t, addresses, storedService.Status.EffectiveAddresses)
	require.Equal(t, apiv2.LogicalServicePhaseReady, storedService.Status.Phase)
	storedService.Status.EffectiveAddresses = nil
	storedService.Annotations = map[string]string{"test": "status-isolation"}
	require.NoError(t, apiClient.Update(ctx, storedService))
	require.NoError(t, apiClient.Get(ctx, proxyless.NamespacedName(), storedService))
	require.Equal(t, addresses, storedService.Status.EffectiveAddresses)
	storedService.Spec.Port++
	immutableErr := apiClient.Update(ctx, storedService)
	require.True(t, apierrors.IsInvalid(immutableErr), "%v", immutableErr)

	var services apiv2.LogicalServiceList
	require.NoError(t, apiClient.List(ctx, &services, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, services.Items, 2)
	var endpoints apiv2.ServiceEndpointList
	require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, endpoints.Items, 1)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalService](t, ctx, apiClient, allocated)
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalService](t, ctx, apiClient, proxyless)
	ctrl_testutil.WaitObjectDeleted[apiv2.ServiceEndpoint](t, ctx, apiClient, endpoint)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
}

// Verifies that the real API rejects invalid service protocols and listener ports,
// cross-namespace endpoint references, and unallocated endpoint ports.
func TestV2ServiceAPIRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-service-invalid-api"}}
	require.NoError(t, client.Create(ctx, namespace))
	waitV2NamespaceActive(t, ctx, namespace.Name)
	for _, testCase := range []struct {
		name string
		spec apiv2.LogicalServiceSpec
	}{
		{"invalid-protocol", apiv2.LogicalServiceSpec{Protocol: "HTTP"}},
		{"invalid-port", apiv2.LogicalServiceSpec{Port: -1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &apiv2.LogicalService{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: namespace.Name},
				Spec:       testCase.spec,
			}
			createErr := client.Create(ctx, service)
			require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
		})
	}
	for _, testCase := range []struct {
		name string
		spec apiv2.ServiceEndpointSpec
	}{
		{"cross-namespace", apiv2.ServiceEndpointSpec{ServiceRef: "other/service", Addresses: []apiv2.ServiceAddress{{Address: "::1", Port: 5000}}}},
		{"unallocated-port", apiv2.ServiceEndpointSpec{ServiceRef: "service", Addresses: []apiv2.ServiceAddress{{Address: "::1"}}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint := &apiv2.ServiceEndpoint{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.name, Namespace: namespace.Name},
				Spec:       testCase.spec,
			}
			createErr := client.Create(ctx, endpoint)
			require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
		})
	}
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
}

// Verifies that the real API rejects dynamically allocated ports for existing processes
// without launching, stopping, or publishing endpoints for the observed identity.
func TestV2LogicalProcessExistingModeRejectsDynamicPorts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	namespace := createActiveV2Namespace(t, ctx, "v2-existing-port-validation")
	handle := seedAdoptablePhysicalProcess(t, ctx, "v2-existing-port-validation-command")
	pid := int64(handle.Pid)
	resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name},
		Spec: apiv2.LogicalProcessSpec{
			Mode: apiv2.LogicalProcessModeExisting, PID: &pid, IdentityTimestamp: metav1.NewMicroTime(handle.IdentityTime),
			Ports: []apiv2.LogicalProcessPort{{LogicalServiceBinding: apiv2.LogicalServiceBinding{Name: "http", ServiceRef: "service"}}},
		}}
	createErr := client.Create(ctx, resource)
	require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
	require.Contains(t, createErr.Error(), "spec.ports[0].port")
	require.NoError(t, testProcessExecutor.CheckProcessRunning(handle))
	var endpoints apiv2.ServiceEndpointList
	require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
	require.Empty(t, endpoints.Items)
	require.NoError(t, client.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, client, namespace)
}

// Verifies that the real API rejects every dynamic persistent process port regardless of lifecycle key,
// while accepting fixed ports and persistent processes without endpoint registrations.
func TestV2LogicalProcessPersistentModeRequiresFixedPorts(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	server, _, startupErr := StartTestEnvironment(t, ctx, NamespaceController, t.Name(), t.TempDir())
	require.NoError(t, startupErr)
	apiClient := server.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v2-persistent-port-validation"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	for _, keyKind := range []string{"computed", "explicit"} {
		for _, testCase := range []struct {
			name         string
			ports        []int32
			invalidField string
		}{
			{name: "dynamic", ports: []int32{0}, invalidField: "spec.ports[0].port"},
			{name: "mixed", ports: []int32{8080, 0}, invalidField: "spec.ports[1].port"},
			{name: "fixed", ports: []int32{1, 65535}},
			{name: "no-ports"},
		} {
			t.Run(keyKind+"/"+testCase.name, func(t *testing.T) {
				spec := apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent,
					Process: &apiv2.LogicalProcessConfig{ExecutablePath: "persistent-port-validation"}}
				if keyKind == "explicit" {
					spec.LifecycleKey = "caller-key"
				}
				for portIndex, port := range testCase.ports {
					spec.Ports = append(spec.Ports, apiv2.LogicalProcessPort{
						LogicalServiceBinding: apiv2.LogicalServiceBinding{Name: []string{"http", "metrics"}[portIndex], ServiceRef: "service"},
						Port:                  port,
					})
				}
				resource := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: keyKind + "-" + testCase.name, Namespace: namespace.Name}, Spec: spec}
				createErr := apiClient.Create(ctx, resource)
				if testCase.invalidField != "" {
					require.True(t, apierrors.IsInvalid(createErr), "%v", createErr)
					require.Contains(t, createErr.Error(), testCase.invalidField)
					require.Contains(t, createErr.Error(), "an explicit serving port is required in persistent mode")
				} else {
					require.NoError(t, createErr)
				}
			})
		}
	}
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
}
