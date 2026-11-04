/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

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
	session := &apiv2.LogicalService{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: namespace.Name},
	}
	require.NoError(t, apiClient.Create(ctx, session))
	addresses := []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 5000}, {Address: "::1", Port: 5001}}
	existing := &apiv2.LogicalService{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: namespace.Name},
		Spec:       apiv2.LogicalServiceSpec{Mode: apiv2.LogicalServiceModeExisting, Addresses: addresses},
	}
	require.NoError(t, apiClient.Create(ctx, existing))
	require.Empty(t, existing.OwnerReferences)
	require.Empty(t, existing.Spec.Address)
	endpoint := &apiv2.ServiceEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: namespace.Name},
		Spec:       apiv2.ServiceEndpointSpec{ServiceRef: session.Name, Addresses: addresses},
	}
	require.NoError(t, apiClient.Create(ctx, endpoint))
	require.Empty(t, endpoint.OwnerReferences)
	storedEndpoint := &apiv2.ServiceEndpoint{}
	require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), storedEndpoint))
	require.Equal(t, addresses, storedEndpoint.Spec.Addresses)
	storedEndpoint.Spec.Addresses[1].Port = 5002
	require.NoError(t, apiClient.Update(ctx, storedEndpoint))
	require.NoError(t, apiClient.Get(ctx, endpoint.NamespacedName(), storedEndpoint))
	require.Equal(t, int32(5002), storedEndpoint.Spec.Addresses[1].Port)

	existing.Status = apiv2.LogicalServiceStatus{Phase: apiv2.LogicalServicePhaseReady, EffectiveAddresses: addresses}
	require.NoError(t, apiClient.Status().Update(ctx, existing))
	storedService := &apiv2.LogicalService{}
	require.NoError(t, apiClient.Get(ctx, existing.NamespacedName(), storedService))
	require.Equal(t, addresses, storedService.Spec.Addresses)
	require.Equal(t, addresses, storedService.Status.EffectiveAddresses)
	require.Equal(t, apiv2.LogicalServicePhaseReady, storedService.Status.Phase)
	storedService.Status.EffectiveAddresses = nil
	storedService.Annotations = map[string]string{"test": "status-isolation"}
	require.NoError(t, apiClient.Update(ctx, storedService))
	require.NoError(t, apiClient.Get(ctx, existing.NamespacedName(), storedService))
	require.Equal(t, addresses, storedService.Status.EffectiveAddresses)
	storedService.Spec.Addresses[0].Port++
	immutableErr := apiClient.Update(ctx, storedService)
	require.True(t, apierrors.IsInvalid(immutableErr), "%v", immutableErr)

	var services apiv2.LogicalServiceList
	require.NoError(t, apiClient.List(ctx, &services, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, services.Items, 2)
	var endpoints apiv2.ServiceEndpointList
	require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, endpoints.Items, 1)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalService](t, ctx, apiClient, session)
	ctrl_testutil.WaitObjectDeleted[apiv2.LogicalService](t, ctx, apiClient, existing)
	ctrl_testutil.WaitObjectDeleted[apiv2.ServiceEndpoint](t, ctx, apiClient, endpoint)
	ctrl_testutil.WaitObjectDeleted[apiv2.Namespace](t, ctx, apiClient, namespace)
}

// Verifies that the real API rejects persistent service policy, missing predefined addresses,
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
		{"persistent", apiv2.LogicalServiceSpec{Mode: "persistent"}},
		{"existing-without-addresses", apiv2.LogicalServiceSpec{Mode: apiv2.LogicalServiceModeExisting}},
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
