/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type failOncePhysicalContainerNetworkStatusClient struct {
	ctrl_client.Client

	lock      sync.Mutex
	failure   error
	triggered bool
}

func (client *failOncePhysicalContainerNetworkStatusClient) Status() ctrl_client.SubResourceWriter {
	return &failOncePhysicalContainerNetworkStatusWriter{
		SubResourceWriter: client.Client.Status(),
		client:            client,
	}
}

func (client *failOncePhysicalContainerNetworkStatusClient) failStatusPatch(obj ctrl_client.Object) error {
	network, isNetwork := obj.(*apiv2.PhysicalContainerNetwork)
	if !isNetwork || network.Status.Phase != apiv2.PhysicalContainerNetworkPhaseFailed {
		return nil
	}

	client.lock.Lock()
	defer client.lock.Unlock()
	if client.triggered {
		return nil
	}

	client.triggered = true
	return client.failure
}

func (client *failOncePhysicalContainerNetworkStatusClient) failureTriggered() bool {
	client.lock.Lock()
	defer client.lock.Unlock()
	return client.triggered
}

type failOncePhysicalContainerNetworkStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *failOncePhysicalContainerNetworkStatusClient
}

func (writer *failOncePhysicalContainerNetworkStatusWriter) Patch(
	ctx context.Context,
	obj ctrl_client.Object,
	patch ctrl_client.Patch,
	opts ...ctrl_client.SubResourcePatchOption,
) error {
	if statusErr := writer.client.failStatusPatch(obj); statusErr != nil {
		return statusErr
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func TestV2PhysicalContainerNetworkControllerRetainsTerminalCreateFailureUntilStatusIsDurable(t *testing.T) {
	testCases := map[string]func(string) error{
		"conflict": func(name string) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: apiv2.GroupName, Resource: "physicalcontainernetworks"},
				name,
				errors.New("simulated status conflict"),
			)
		},
		"save error": func(_ string) error {
			return errors.New("simulated status save failure")
		},
	}

	for name, makeFailure := range testCases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()

			scheme := runtime.NewScheme()
			require.NoError(t, apiv2.AddToScheme(scheme))

			namespace := &apiv2.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "durable-create-failure",
					Finalizers: []string{apiv2.NamespaceFinalizer},
				},
				Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
			}
			networkName := "durable-create-failure-runtime"
			network := &apiv2.PhysicalContainerNetwork{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "durable-create-failure",
					Namespace:  namespace.Name,
					UID:        types.UID("durable-create-failure"),
					Finalizers: []string{apiv2.GroupName + "/physicalcontainernetwork-reconciler"},
				},
				Spec: apiv2.PhysicalContainerNetworkSpec{NetworkName: networkName},
			}

			baseClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerNetwork{}).
				WithObjects(namespace, network).
				Build()
			statusClient := &failOncePhysicalContainerNetworkStatusClient{
				Client:  baseClient,
				failure: makeFailure(network.Name),
			}

			log := testutil.NewLogForTesting(t.Name())
			orchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(
				ctx,
				log,
				ctrl_testutil.TcoOptionNone,
			)
			require.NoError(t, orchestratorErr)
			defer func() {
				require.NoError(t, orchestrator.Close())
			}()

			_, createErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{Name: networkName})
			require.NoError(t, createErr)

			reconciler := controllers.NewPhysicalContainerNetworkReconciler(
				ctx,
				statusClient,
				baseClient,
				log,
				orchestrator,
			)
			request := ctrl.Request{NamespacedName: network.NamespacedName()}

			_, reconcileErr := reconciler.Reconcile(ctx, request)
			require.NoError(t, reconcileErr)

			waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
				if orchestrator.CreateNetworkCallCount(networkName) < 2 {
					return false, nil
				}

				_, currentReconcileErr := reconciler.Reconcile(ctx, request)
				if statusClient.failureTriggered() {
					return true, nil
				}
				return false, currentReconcileErr
			})
			require.NoError(t, waitErr)
			require.Equal(t, 2, orchestrator.CreateNetworkCallCount(networkName))

			waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
				_, currentReconcileErr := reconciler.Reconcile(ctx, request)
				if currentReconcileErr != nil {
					return false, currentReconcileErr
				}

				currentNetwork := &apiv2.PhysicalContainerNetwork{}
				if getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
					return false, getErr
				}
				return currentNetwork.Status.Phase == apiv2.PhysicalContainerNetworkPhaseFailed, nil
			})
			require.NoError(t, waitErr)
			require.Equal(t, 2, orchestrator.CreateNetworkCallCount(networkName))
		})
	}
}
