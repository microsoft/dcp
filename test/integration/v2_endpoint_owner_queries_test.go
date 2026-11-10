/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type endpointOwnerQueryClient struct {
	ctrl_client.Client
	ownerField string
	queries    atomic.Int32
	invalid    atomic.Int32
	hideOwned  atomic.Bool
	injected   atomic.Bool
	foreign    atomic.Pointer[apiv2.ServiceEndpoint]
}

func (apiClient *endpointOwnerQueryClient) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	endpoints, endpointQuery := objects.(*apiv2.ServiceEndpointList)
	if !endpointQuery {
		return apiClient.Client.List(ctx, objects, options...)
	}
	if listOptions.FieldSelector == nil {
		apiClient.invalid.Add(1)
		return errors.New("unindexed cached endpoint query")
	}
	ownerUID, ownerQuery := listOptions.FieldSelector.RequiresExactMatch(apiClient.ownerField)
	listErr := apiClient.Client.List(ctx, objects, options...)
	if listErr != nil || !ownerQuery {
		return listErr
	}
	apiClient.queries.Add(1)
	for _, endpoint := range endpoints.Items {
		owner := metav1.GetControllerOf(&endpoint)
		if owner == nil || string(owner.UID) != ownerUID {
			apiClient.invalid.Add(1)
			return errors.New("owner index returned an unrelated endpoint")
		}
	}
	if apiClient.hideOwned.Load() {
		endpoints.Items = slices.DeleteFunc(endpoints.Items, func(endpoint apiv2.ServiceEndpoint) bool {
			return endpoint.Name != "obsolete"
		})
	}
	return nil
}

func (apiClient *endpointOwnerQueryClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	endpoint, endpointCreate := object.(*apiv2.ServiceEndpoint)
	if !endpointCreate || !apiClient.injected.CompareAndSwap(false, true) {
		return apiClient.Client.Create(ctx, object, options...)
	}
	foreign := endpoint.DeepCopy()
	foreign.OwnerReferences[0].UID = "another-owner"
	createErr := apiClient.Client.Create(ctx, foreign, options...)
	if createErr != nil {
		return createErr
	}
	apiClient.foreign.Store(foreign)
	return apierrors.NewAlreadyExists(apiv2.GroupVersion.WithResource("serviceendpoints").GroupResource(), foreign.Name)
}

type endpointAbsenceReader struct {
	ctrl_client.Reader
	allow         atomic.Bool
	fail          atomic.Bool
	unexpected    atomic.Int32
	confirmations atomic.Int32
}

func (reader *endpointAbsenceReader) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, endpoints := objects.(*apiv2.ServiceEndpointList); endpoints {
		reader.unexpected.Add(1)
		return errors.New("unscoped uncached endpoint list")
	}
	return reader.Reader.List(ctx, objects, options...)
}

func (reader *endpointAbsenceReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	if _, endpoint := object.(*apiv2.ServiceEndpoint); endpoint && reader.allow.Load() {
		reader.confirmations.Add(1)
		if reader.fail.Load() {
			return errors.New("endpoint absence confirmation unavailable")
		}
	}
	return reader.Reader.Get(ctx, key, object, options...)
}

// Verifies that logical workloads query cached endpoints by owner UID, reject foreign name
// collisions, preserve unrelated endpoints, and confirm unobserved creations by name before finalizing.
func TestV2LogicalEndpointOwnerQueries(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			ownerField := ".metadata.logicalProcessOwnerUID"
			if kind == "container" {
				ownerField = ".metadata.logicalContainerOwnerUID"
			}
			var indexed *endpointOwnerQueryClient
			var reader *endpointAbsenceReader
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						indexed = &endpointOwnerQueryClient{Client: apiClient, ownerField: ownerField}
						return indexed
					},
					DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
						reader = &endpointAbsenceReader{Reader: apiReader}
						return reader
					},
				})
			require.NoError(t, startErr)
			apiClient := server.Client
			namespace := createV2ExecNamespace(t, ctx, apiClient, "v2-endpoint-owner-query")
			t.Cleanup(func() { reader.allow.Store(true); reader.fail.Store(false); indexed.hideOwned.Store(false) })
			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: namespace.Name},
				Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
			require.NoError(t, apiClient.Create(ctx, service))
			binding := apiv2.LogicalServiceBinding{Name: "http", ServiceRef: service.Name}
			metadata := metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name}
			var workload ctrl_client.Object
			if kind == "process" {
				workload = &apiv2.LogicalProcess{ObjectMeta: metadata, Spec: apiv2.LogicalProcessSpec{
					Process: &apiv2.LogicalProcessConfig{ExecutablePath: "endpoint-owner-query-process"},
					Ports:   []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Port: 8080}},
				}}
			} else {
				workload = &apiv2.LogicalContainer{ObjectMeta: metadata, Spec: apiv2.LogicalContainerSpec{
					Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "endpoint-owner-query-image"}},
					Ports:     []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}},
				}}
			}
			require.NoError(t, apiClient.Create(ctx, workload))
			waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(_ *apiv2.LogicalService) (bool, error) {
				return indexed.foreign.Load() != nil && indexed.queries.Load() > 0, nil
			})
			collision := indexed.foreign.Load()
			stored := &apiv2.ServiceEndpoint{}
			require.NoError(t, apiClient.Get(ctx, collision.NamespacedName(), stored))
			require.Equal(t, collision.UID, stored.UID)
			require.Equal(t, collision.OwnerReferences, stored.OwnerReferences)
			require.False(t, metav1.IsControlledBy(stored, workload))
			workloadName := ctrl_client.ObjectKeyFromObject(workload)
			if kind == "process" {
				waitObjectAssumesStateEx(t, ctx, apiClient, workloadName, func(current *apiv2.LogicalProcess) (bool, error) {
					condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return current.Status.Phase == apiv2.LogicalProcessPhasePending && condition != nil &&
						condition.Reason == string(apiv2.ConditionReason("Starting")), nil
				})
			} else {
				waitObjectAssumesStateEx(t, ctx, apiClient, workloadName, func(current *apiv2.LogicalContainer) (bool, error) {
					condition := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return current.Status.Phase == apiv2.LogicalContainerPhasePending && condition != nil &&
						condition.Reason == string(apiv2.ConditionReason("Starting")), nil
				})
			}
			indexed.hideOwned.Store(true)
			require.NoError(t, apiClient.Delete(ctx, stored))
			waitObjectAssumesStateEx(t, ctx, apiClient, collision.NamespacedName(), func(current *apiv2.ServiceEndpoint) (bool, error) {
				return metav1.IsControlledBy(current, workload), nil
			})
			ready := waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return logicalServiceReady(current), nil
			})
			require.NotEmpty(t, ready.Status.EffectiveAddresses)
			require.NoError(t, apiClient.Get(ctx, collision.NamespacedName(), stored))
			require.True(t, metav1.IsControlledBy(stored, workload))
			require.Zero(t, indexed.invalid.Load())
			require.Zero(t, reader.unexpected.Load())
			unrelated := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name}, Spec: stored.Spec}
			require.NoError(t, apiClient.Create(ctx, unrelated))
			held := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{
				Name: "obsolete", Namespace: namespace.Name, OwnerReferences: stored.OwnerReferences,
				Finalizers: []string{"tests.dcp.microsoft.com/hold"},
			}, Spec: stored.Spec}
			require.NoError(t, apiClient.Create(ctx, held))
			t.Cleanup(func() {
				release := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: held.Name, Namespace: held.Namespace}}
				require.NoError(t, ctrl_client.IgnoreNotFound(apiClient.Patch(ctx, release,
					ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":[]}}`)))))
			})
			reader.allow.Store(true)
			reader.fail.Store(true)
			require.NoError(t, apiClient.Delete(ctx, workload))
			waitObjectAssumesStateEx(t, ctx, apiClient, service.NamespacedName(), func(_ *apiv2.LogicalService) (bool, error) {
				return reader.confirmations.Load() > 0, nil
			})
			require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload), workload))
			require.False(t, workload.GetDeletionTimestamp().IsZero())
			reader.fail.Store(false)
			waitObjectAssumesStateEx(t, ctx, apiClient, held.NamespacedName(), func(current *apiv2.ServiceEndpoint) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload), workload))
			require.NoError(t, apiClient.Get(ctx, held.NamespacedName(), held))
			held.Finalizers = nil
			require.NoError(t, apiClient.Update(ctx, held))
			if logicalProcess, isProcess := workload.(*apiv2.LogicalProcess); isProcess {
				ctrl_testutil.WaitObjectDeleted[apiv2.LogicalProcess](t, ctx, apiClient, logicalProcess)
			} else {
				ctrl_testutil.WaitObjectDeleted[apiv2.LogicalContainer](t, ctx, apiClient, workload.(*apiv2.LogicalContainer))
			}
			require.NoError(t, apiClient.Get(ctx, unrelated.NamespacedName(), unrelated))
			require.True(t, unrelated.DeletionTimestamp.IsZero())
			require.Zero(t, indexed.invalid.Load())
			require.Zero(t, reader.unexpected.Load())
		})
	}
}
