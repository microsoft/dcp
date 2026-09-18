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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

	lock       sync.Mutex
	failure    error
	shouldFail func(*apiv2.PhysicalContainerNetwork) bool
	triggered  bool
}

func (client *failOncePhysicalContainerNetworkStatusClient) Status() ctrl_client.SubResourceWriter {
	return &failOncePhysicalContainerNetworkStatusWriter{
		SubResourceWriter: client.Client.Status(),
		client:            client,
	}
}

func (client *failOncePhysicalContainerNetworkStatusClient) failStatusPatch(obj ctrl_client.Object) error {
	network, isNetwork := obj.(*apiv2.PhysicalContainerNetwork)
	if !isNetwork || !client.shouldFail(network) {
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

type blockingNetworkInspectionOrchestrator struct {
	containers.NetworkAttachmentOrchestrator

	inspectionStarted chan struct{}
	releaseInspection chan struct{}
	startOnce         sync.Once
	releaseOnce       sync.Once
}

type failingNetworkInspectionOrchestrator struct {
	containers.NetworkAttachmentOrchestrator

	lock             sync.Mutex
	inspectionErrors []error
}

func (orchestrator *failingNetworkInspectionOrchestrator) InspectNetworks(
	ctx context.Context,
	options containers.InspectNetworksOptions,
) ([]containers.InspectedNetwork, error) {
	orchestrator.lock.Lock()
	if len(orchestrator.inspectionErrors) > 0 {
		inspectErr := orchestrator.inspectionErrors[0]
		orchestrator.inspectionErrors = orchestrator.inspectionErrors[1:]
		orchestrator.lock.Unlock()
		return nil, inspectErr
	}
	orchestrator.lock.Unlock()
	return orchestrator.NetworkAttachmentOrchestrator.InspectNetworks(ctx, options)
}

func (orchestrator *blockingNetworkInspectionOrchestrator) InspectNetworks(
	ctx context.Context,
	options containers.InspectNetworksOptions,
) ([]containers.InspectedNetwork, error) {
	orchestrator.startOnce.Do(func() {
		close(orchestrator.inspectionStarted)
	})
	select {
	case <-orchestrator.releaseInspection:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return orchestrator.NetworkAttachmentOrchestrator.InspectNetworks(ctx, options)
}

func (orchestrator *blockingNetworkInspectionOrchestrator) release() {
	orchestrator.releaseOnce.Do(func() {
		close(orchestrator.releaseInspection)
	})
}

func TestV2PhysicalContainerNetworkControllerQueuesDeletionBeforeRemovingFinalizer(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	baseOrchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, baseOrchestrator.Close())
	}()
	networkName := "queued-network-deletion-runtime"
	networkID, createErr := baseOrchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: networkName,
		Labels: map[string]string{
			"com.microsoft.developer.usvc-dev.uid": "queued-network-deletion",
		},
	})
	require.NoError(t, createErr)

	orchestrator := &blockingNetworkInspectionOrchestrator{
		NetworkAttachmentOrchestrator: baseOrchestrator,
		inspectionStarted:             make(chan struct{}),
		releaseInspection:             make(chan struct{}),
	}
	defer orchestrator.release()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	now := metav1.Now()
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "queued-network-deletion",
			Namespace:         "queued-network-deletion",
			UID:               types.UID("queued-network-deletion"),
			Finalizers:        []string{apiv2.GroupName + "/physicalcontainernetwork-reconciler"},
			DeletionTimestamp: &now,
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			Network: &apiv2.PhysicalContainerNetworkConfig{NetworkName: networkName},
		},
		Status: apiv2.PhysicalContainerNetworkStatus{
			NetworkID: networkID,
			Phase:     apiv2.PhysicalContainerNetworkPhaseReady,
		},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.PhysicalContainerNetwork{}).
		WithObjects(network).
		Build()
	reconciler := controllers.NewPhysicalContainerNetworkReconciler(ctx, baseClient, baseClient, log, orchestrator)
	request := ctrl.Request{NamespacedName: network.NamespacedName()}

	reconcileDone := make(chan error, 1)
	go func() {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		reconcileDone <- reconcileErr
	}()

	select {
	case <-orchestrator.inspectionStarted:
	case <-ctx.Done():
		require.FailNow(t, "runtime network deletion did not start")
	}
	select {
	case reconcileErr := <-reconcileDone:
		require.NoError(t, reconcileErr)
	case <-ctx.Done():
		require.FailNow(t, "reconciliation blocked on runtime network deletion")
	}

	currentNetwork := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, baseClient.Get(ctx, network.NamespacedName(), currentNetwork))
	require.NotEmpty(t, currentNetwork.Finalizers)

	orchestrator.release()
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork)
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, getErr
		}
		return len(currentNetwork.Finalizers) == 0, nil
	})
	require.NoError(t, waitErr)

	_, inspectErr := baseOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkID}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

func TestV2PhysicalContainerNetworkControllerRetriesUncertainCreateCleanup(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "uncertain-create-cleanup",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	networkName := "uncertain-create-cleanup-runtime"
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "uncertain-create-cleanup",
			Namespace:  namespace.Name,
			UID:        types.UID("uncertain-create-cleanup"),
			Finalizers: []string{apiv2.GroupName + "/physicalcontainernetwork-reconciler"},
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			Network: &apiv2.PhysicalContainerNetworkConfig{NetworkName: networkName},
		},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerNetwork{}).
		WithObjects(namespace, network).
		Build()
	log := testutil.NewLogForTesting(t.Name())
	baseOrchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(ctx, log, ctrl_testutil.TcoOptionNone)
	require.NoError(t, orchestratorErr)
	defer func() {
		require.NoError(t, baseOrchestrator.Close())
	}()
	baseOrchestrator.FailNextCreateNetworkAfterCreation(networkName, errors.New("create result lost"))
	orchestrator := &failingNetworkInspectionOrchestrator{
		NetworkAttachmentOrchestrator: baseOrchestrator,
		inspectionErrors: []error{
			errors.New("create verification unavailable"),
			errors.New("deletion verification unavailable"),
		},
	}

	reconciler := controllers.NewPhysicalContainerNetworkReconciler(ctx, baseClient, baseClient, log, orchestrator)
	request := ctrl.Request{NamespacedName: network.NamespacedName()}
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		currentNetwork := &apiv2.PhysicalContainerNetwork{}
		if getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false, getErr
		}
		readyCondition := apimeta.FindStatusCondition(currentNetwork.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerNetworkReasonCreateFailed), nil
	})
	require.NoError(t, waitErr)

	currentNetwork := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, baseClient.Get(ctx, network.NamespacedName(), currentNetwork))
	require.NoError(t, baseClient.Delete(ctx, currentNetwork))

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		if getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false, getErr
		}
		readyCondition := apimeta.FindStatusCondition(currentNetwork.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerNetworkReasonRuntimeNetworkRemoveFailed), nil
	})
	require.NoError(t, waitErr)
	require.NotEmpty(t, currentNetwork.Finalizers)

	inspectedNetworks, inspectErr := baseOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkName}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedNetworks, 1)

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork)
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, getErr
		}
		return len(currentNetwork.Finalizers) == 0, nil
	})
	require.NoError(t, waitErr)

	_, inspectErr = baseOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{networkName}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
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
				Spec: apiv2.PhysicalContainerNetworkSpec{
					Network: &apiv2.PhysicalContainerNetworkConfig{NetworkName: networkName},
				},
			}

			baseClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerNetwork{}).
				WithObjects(namespace, network).
				Build()
			statusClient := &failOncePhysicalContainerNetworkStatusClient{
				Client:  baseClient,
				failure: makeFailure(network.Name),
				shouldFail: func(network *apiv2.PhysicalContainerNetwork) bool {
					return network.Status.Phase == apiv2.PhysicalContainerNetworkPhaseFailed
				},
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

func TestV2PhysicalContainerNetworkControllerRetainsBuiltInFailureUntilStatusIsDurable(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "durable-built-in-failure",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "durable-built-in-failure",
			Namespace:  namespace.Name,
			UID:        types.UID("durable-built-in-failure"),
			Finalizers: []string{apiv2.GroupName + "/physicalcontainernetwork-reconciler"},
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			Network: &apiv2.PhysicalContainerNetworkConfig{
				NetworkName:     "bridge",
				ReplaceExisting: true,
			},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerNetwork{}).
		WithObjects(namespace, network).
		Build()
	statusClient := &failOncePhysicalContainerNetworkStatusClient{
		Client:  baseClient,
		failure: errors.New("simulated status save failure"),
		shouldFail: func(network *apiv2.PhysicalContainerNetwork) bool {
			return network.Status.Phase == apiv2.PhysicalContainerNetworkPhaseFailed
		},
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
		_, currentReconcileErr := reconciler.Reconcile(ctx, request)
		if statusClient.failureTriggered() {
			return true, nil
		}
		return false, currentReconcileErr
	})
	require.NoError(t, waitErr)
	require.Equal(t, 0, orchestrator.CreateNetworkCallCount(network.Spec.Network.NetworkName))

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, currentReconcileErr := reconciler.Reconcile(ctx, request)
		if currentReconcileErr != nil {
			return false, currentReconcileErr
		}

		currentNetwork := &apiv2.PhysicalContainerNetwork{}
		if getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false, getErr
		}
		readyCondition := apimeta.FindStatusCondition(currentNetwork.Status.Conditions, string(apiv2.ConditionReady))
		return currentNetwork.Status.Phase == apiv2.PhysicalContainerNetworkPhaseFailed &&
			readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerNetworkReasonBuiltInNetworkNotRemovable), nil
	})
	require.NoError(t, waitErr)
	require.Equal(t, 0, orchestrator.CreateNetworkCallCount(network.Spec.Network.NetworkName))
}

func TestV2PhysicalContainerNetworkControllerAdoptsOwnedNetworkBeforeReplacement(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "replace-adopts-owned-network",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "replace-adopts-owned-network",
			Namespace:  namespace.Name,
			UID:        types.UID("replace-adopts-owned-network"),
			Finalizers: []string{apiv2.GroupName + "/physicalcontainernetwork-reconciler"},
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			Network: &apiv2.PhysicalContainerNetworkConfig{
				NetworkName:     "replace-adopts-owned-network-runtime",
				ReplaceExisting: true,
			},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerNetwork{}).
		WithObjects(namespace, network).
		Build()

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

	ownedNetworkID, createErr := orchestrator.CreateNetwork(ctx, containers.CreateNetworkOptions{
		Name: network.Spec.Network.NetworkName,
		Labels: map[string]string{
			"com.microsoft.developer.usvc-dev.uid": string(network.UID),
		},
	})
	require.NoError(t, createErr)

	reconciler := controllers.NewPhysicalContainerNetworkReconciler(
		ctx,
		baseClient,
		baseClient,
		log,
		orchestrator,
	)
	request := ctrl.Request{NamespacedName: network.NamespacedName()}

	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		currentNetwork := &apiv2.PhysicalContainerNetwork{}
		if getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false, getErr
		}
		return currentNetwork.Status.Phase == apiv2.PhysicalContainerNetworkPhaseReady &&
			currentNetwork.Status.NetworkID == ownedNetworkID, nil
	})
	require.NoError(t, waitErr)
	require.Equal(t, 1, orchestrator.CreateNetworkCallCount(network.Spec.Network.NetworkName))
}

func TestV2PhysicalContainerNetworkControllerRetainsCreatedNetworkUntilStatusIsDurable(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))

	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "durable-created-network",
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
	network := &apiv2.PhysicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "durable-created-network",
			Namespace:  namespace.Name,
			UID:        types.UID("durable-created-network"),
			Finalizers: []string{apiv2.GroupName + "/physicalcontainernetwork-reconciler"},
		},
		Spec: apiv2.PhysicalContainerNetworkSpec{
			Network: &apiv2.PhysicalContainerNetworkConfig{NetworkName: "durable-created-network-runtime"},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerNetwork{}).
		WithObjects(namespace, network).
		Build()
	statusClient := &failOncePhysicalContainerNetworkStatusClient{
		Client:  baseClient,
		failure: errors.New("simulated ready status save failure"),
		shouldFail: func(network *apiv2.PhysicalContainerNetwork) bool {
			return network.Status.Phase == apiv2.PhysicalContainerNetworkPhaseReady
		},
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
		_, currentReconcileErr := reconciler.Reconcile(ctx, request)
		if statusClient.failureTriggered() {
			return true, nil
		}
		return false, currentReconcileErr
	})
	require.NoError(t, waitErr)
	require.Equal(t, 1, orchestrator.CreateNetworkCallCount(network.Spec.Network.NetworkName))

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, currentReconcileErr := reconciler.Reconcile(ctx, request)
		if currentReconcileErr != nil {
			return false, currentReconcileErr
		}

		currentNetwork := &apiv2.PhysicalContainerNetwork{}
		if getErr := baseClient.Get(ctx, network.NamespacedName(), currentNetwork); getErr != nil {
			return false, getErr
		}
		return currentNetwork.Status.Phase == apiv2.PhysicalContainerNetworkPhaseReady, nil
	})
	require.NoError(t, waitErr)
	require.Equal(t, 1, orchestrator.CreateNetworkCallCount(network.Spec.Network.NetworkName))
}
