/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/dcpclient"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/internal/statestore"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/syncmap"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestStopServiceReleasesProxyPortReservations(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	log := logr.Discard()
	store, owner := configureTestPortAllocator(t, ctx)

	const address = networking.IPv4LocalhostDefaultAddress
	const port int32 = 26030
	reserveTestPort(t, ctx, store, owner, apiv1.TCP, address, port)
	requirePortReserved(t, ctx, store, apiv1.TCP, address, port, owner)

	serviceName := types.NamespacedName{Name: "test-service", Namespace: metav1.NamespaceNone}
	stopped := false
	r := &ServiceReconciler{
		serviceInfo: &syncmap.Map[types.NamespacedName, *serviceData]{},
	}
	r.serviceInfo.Store(serviceName, &serviceData{
		proxies: []proxyInstanceData{
			{
				stopProxy:               func() { stopped = true },
				portReservationProtocol: apiv1.TCP,
				portReservationAddress:  address,
				portReservationPort:     port,
			},
		},
	})

	change := r.stopService(ctx, serviceName, nil, log)

	require.Equal(t, noChange, change)
	require.True(t, stopped)
	_, found := r.serviceInfo.Load(serviceName)
	require.False(t, found)
	requirePortReleased(t, ctx, store, apiv1.TCP, address, port, owner)
}

func TestStopServiceMarksProxyDataStoppedWhenReservationReleaseFails(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	log := logr.Discard()
	configureTestPortAllocator(t, ctx)

	serviceName := types.NamespacedName{Name: "test-service-release-failure", Namespace: metav1.NamespaceNone}
	stopped := false
	r := &ServiceReconciler{
		serviceInfo: &syncmap.Map[types.NamespacedName, *serviceData]{},
	}
	r.serviceInfo.Store(serviceName, &serviceData{
		proxies: []proxyInstanceData{
			{
				stopProxy:               func() { stopped = true },
				portReservationProtocol: apiv1.TCP,
				portReservationAddress:  networking.IPv4LocalhostDefaultAddress,
				portReservationPort:     networking.InvalidPort,
			},
		},
	})

	change := r.stopService(ctx, serviceName, nil, log)

	require.Equal(t, additionalReconciliationNeeded, change)
	require.True(t, stopped)
	updatedServiceData, found := r.serviceInfo.Load(serviceName)
	require.True(t, found)
	require.True(t, updatedServiceData.proxiesStopped)
}

func TestServiceDeletionReleasesEndpointPortReservations(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	log := logr.Discard()
	store, owner := configureTestPortAllocator(t, ctx)

	const address = networking.IPv4LocalhostDefaultAddress
	const port int32 = 26031
	svc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxyless-service",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol:              apiv1.TCP,
			AddressAllocationMode: apiv1.AddressAllocationModeProxyless,
		},
	}
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxyless-endpoint",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.Namespace,
			ServiceName:      svc.Name,
			Address:          address,
			Port:             port,
		},
	}
	reserveTestPort(t, ctx, store, owner, svc.Spec.Protocol, endpoint.Spec.Address, endpoint.Spec.Port)
	requirePortReserved(t, ctx, store, svc.Spec.Protocol, endpoint.Spec.Address, endpoint.Spec.Port, owner)

	apiClient := fake.NewClientBuilder().
		WithScheme(dcpclient.NewScheme()).
		WithObjects(svc, endpoint).
		WithIndex(&apiv1.Endpoint{}, serviceNamespaceKey, func(rawObj ctrl_client.Object) []string {
			indexedEndpoint := rawObj.(*apiv1.Endpoint)
			return []string{indexedEndpoint.Spec.ServiceNamespace}
		}).
		WithIndex(&apiv1.Endpoint{}, serviceNameKey, func(rawObj ctrl_client.Object) []string {
			indexedEndpoint := rawObj.(*apiv1.Endpoint)
			return []string{indexedEndpoint.Spec.ServiceName}
		}).
		Build()
	processExecutor := internal_testutil.NewTestProcessExecutor(ctx)
	defer processExecutor.Dispose()
	r := NewServiceReconciler(ctx, apiClient, apiClient, log, ServiceReconcilerConfig{ProcessExecutor: processExecutor})

	change := r.releaseServiceEndpointPortReservations(ctx, svc, log)

	require.Equal(t, noChange, change)
	requirePortReleased(t, ctx, store, svc.Spec.Protocol, endpoint.Spec.Address, endpoint.Spec.Port, owner)
}

func TestEnsureEndpointsForWorkloadReleasesReservationWhenCreateFails(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	log := logr.Discard()
	store, owner := configureTestPortAllocator(t, ctx)

	const serviceName = "test-endpoint-create-failure-service"
	const address = networking.IPv4LocalhostDefaultAddress
	const port int32 = 26032
	svc := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ServiceSpec{
			Protocol: apiv1.TCP,
		},
	}
	workload := &apiv1.Container{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1.GroupVersion.String(),
			Kind:       "Container",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-endpoint-create-failure-container",
			Namespace: metav1.NamespaceNone,
			UID:       "test-endpoint-create-failure-container",
			Annotations: map[string]string{
				commonapi.ServiceProducerAnnotation: `[{"serviceName":"` + serviceName + `","port":80}]`,
			},
		},
	}
	endpoint := &apiv1.Endpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-endpoint-create-failure-endpoint",
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.EndpointSpec{
			ServiceNamespace: svc.Namespace,
			ServiceName:      svc.Name,
			Address:          address,
			Port:             port,
		},
	}
	apiClient := fake.NewClientBuilder().
		WithScheme(dcpclient.NewScheme()).
		WithObjects(svc).
		WithIndex(&apiv1.Endpoint{}, commonapi.WorkloadOwnerKey, func(rawObj ctrl_client.Object) []string {
			indexedEndpoint := rawObj.(*apiv1.Endpoint)
			owners := indexedEndpoint.GetOwnerReferences()
			ownerUIDs := make([]string, 0, len(owners))
			for _, ownerReference := range owners {
				ownerUIDs = append(ownerUIDs, string(ownerReference.UID))
			}
			return ownerUIDs
		}).
		Build()
	createErr := errors.New("create failed")
	endpointOwner := &createFailingEndpointOwner{
		Client: apiClient,
		endpointFactory: func(ctx context.Context, _ ctrl_client.Object, _ commonapi.ServiceProducer, _ []*apiv1.Endpoint, _ struct{}, _ logr.Logger) ([]*apiv1.Endpoint, error) {
			reserveTestPort(t, ctx, store, owner, svc.Spec.Protocol, endpoint.Spec.Address, endpoint.Spec.Port)
			return []*apiv1.Endpoint{endpoint}, nil
		},
		createErr: createErr,
	}

	ensureEndpointsForWorkload(ctx, endpointOwner, workload, nil, struct{}{}, log)

	requirePortReleased(t, ctx, store, svc.Spec.Protocol, endpoint.Spec.Address, endpoint.Spec.Port, owner)
}

type createFailingEndpointOwner struct {
	ctrl_client.Client
	endpointFactory func(context.Context, ctrl_client.Object, commonapi.ServiceProducer, []*apiv1.Endpoint, struct{}, logr.Logger) ([]*apiv1.Endpoint, error)
	createErr       error
}

func (o *createFailingEndpointOwner) Create(ctx context.Context, obj ctrl_client.Object, opts ...ctrl_client.CreateOption) error {
	return o.createErr
}

func (o *createFailingEndpointOwner) createEndpoints(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	existingEndpoints []*apiv1.Endpoint,
	ecc struct{},
	log logr.Logger,
) ([]*apiv1.Endpoint, error) {
	return o.endpointFactory(ctx, owner, serviceProducer, existingEndpoints, ecc, log)
}

func (o *createFailingEndpointOwner) validateExistingEndpoints(
	ctx context.Context,
	owner ctrl_client.Object,
	serviceProducer commonapi.ServiceProducer,
	endpoints []*apiv1.Endpoint,
	ecc struct{},
	log logr.Logger,
) ([]*apiv1.Endpoint, []*apiv1.Endpoint, error) {
	return endpoints, nil, nil
}

func configureTestPortAllocator(t *testing.T, ctx context.Context) (*statestore.Store, process.ProcessTreeItem) {
	t.Helper()

	storePath := filepath.Join(t.TempDir(), "state.sqlite3")
	require.NoError(t, usvc_io.EnsureRestrictedDirectory(filepath.Dir(storePath), osutil.PermissionOnlyOwnerReadWriteTraverse))
	store, openErr := statestore.Open(ctx, statestore.Options{Path: storePath})
	require.NoError(t, openErr)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	owner, ownerErr := statestore.CurrentResourceLeaseOwner()
	require.NoError(t, ownerErr)
	networking.ConfigureStateStorePortAllocator(store, owner)
	t.Cleanup(func() {
		networking.ConfigureStateStorePortAllocator(nil, process.ProcessTreeItem{})
	})
	t.Setenv(networking.DCP_PORT_ALLOCATOR, "statestore")
	return store, owner
}

func reserveTestPort(
	t *testing.T,
	ctx context.Context,
	store *statestore.Store,
	owner process.ProcessTreeItem,
	protocol apiv1.PortProtocol,
	address string,
	port int32,
) {
	t.Helper()

	_, reserveErr := store.ReserveSpecificPort(ctx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: owner,
	})
	require.NoError(t, reserveErr)
}

func requirePortReserved(
	t *testing.T,
	ctx context.Context,
	store *statestore.Store,
	protocol apiv1.PortProtocol,
	address string,
	port int32,
	owner process.ProcessTreeItem,
) {
	t.Helper()

	otherOwner := owner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(time.Second)
	_, reserveErr := store.ReservePort(ctx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: otherOwner,
	})
	require.ErrorIs(t, reserveErr, statestore.ErrPortReservationHeld)
}

func requirePortReleased(
	t *testing.T,
	ctx context.Context,
	store *statestore.Store,
	protocol apiv1.PortProtocol,
	address string,
	port int32,
	owner process.ProcessTreeItem,
) {
	t.Helper()

	otherOwner := owner
	otherOwner.IdentityTime = otherOwner.IdentityTime.Add(time.Second)
	_, reserveErr := store.ReservePort(ctx, statestore.PortReservationRequest{
		Protocol:     string(protocol),
		Address:      address,
		Port:         port,
		OwnerProcess: otherOwner,
	})
	require.NoError(t, reserveErr)
}
