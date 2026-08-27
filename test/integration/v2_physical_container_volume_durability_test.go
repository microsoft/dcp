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

type failOncePhysicalContainerVolumeStatusClient struct {
	ctrl_client.Client

	lock       sync.Mutex
	failure    error
	shouldFail func(*apiv2.PhysicalContainerVolume) bool
	triggered  bool
}

func (client *failOncePhysicalContainerVolumeStatusClient) Status() ctrl_client.SubResourceWriter {
	return &failOncePhysicalContainerVolumeStatusWriter{
		SubResourceWriter: client.Client.Status(),
		client:            client,
	}
}

func (client *failOncePhysicalContainerVolumeStatusClient) failStatusPatch(obj ctrl_client.Object) error {
	volume, isVolume := obj.(*apiv2.PhysicalContainerVolume)
	if !isVolume || !client.shouldFail(volume) {
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

func (client *failOncePhysicalContainerVolumeStatusClient) failureTriggered() bool {
	client.lock.Lock()
	defer client.lock.Unlock()
	return client.triggered
}

type failOncePhysicalContainerVolumeStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *failOncePhysicalContainerVolumeStatusClient
}

type failingVolumeInspectionOrchestrator struct {
	containers.VolumeOrchestrator

	lock             sync.Mutex
	inspectionErrors []error
}

func (orchestrator *failingVolumeInspectionOrchestrator) InspectVolumes(
	ctx context.Context,
	options containers.InspectVolumesOptions,
) ([]containers.InspectedVolume, error) {
	orchestrator.lock.Lock()
	if len(orchestrator.inspectionErrors) > 0 {
		inspectErr := orchestrator.inspectionErrors[0]
		orchestrator.inspectionErrors = orchestrator.inspectionErrors[1:]
		orchestrator.lock.Unlock()
		return nil, inspectErr
	}
	orchestrator.lock.Unlock()
	return orchestrator.VolumeOrchestrator.InspectVolumes(ctx, options)
}

func (writer *failOncePhysicalContainerVolumeStatusWriter) Patch(
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

func TestV2PhysicalContainerVolumeControllerRetriesUncertainCreateCleanup(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	namespace := durablePhysicalContainerVolumeNamespace("uncertain-volume-create-cleanup")
	volumeName := "uncertain-volume-create-cleanup-runtime"
	volume := durablePhysicalContainerVolume(namespace.Name, "uncertain-volume-create-cleanup", volumeName)
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerVolume{}).
		WithObjects(namespace, volume).
		Build()
	baseOrchestrator := newDurabilityTestContainerOrchestrator(t, ctx)
	baseOrchestrator.FailNextCreateVolumeAfterCreation(volumeName, errors.New("create result lost"))
	orchestrator := &failingVolumeInspectionOrchestrator{
		VolumeOrchestrator: baseOrchestrator,
		inspectionErrors: []error{
			errors.New("create verification unavailable"),
			errors.New("deletion verification unavailable"),
		},
	}

	reconciler := controllers.NewPhysicalContainerVolumeReconciler(ctx, baseClient, baseClient, testutil.NewLogForTesting(t.Name()), orchestrator)
	request := ctrl.Request{NamespacedName: volume.NamespacedName()}
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		currentVolume := &apiv2.PhysicalContainerVolume{}
		if getErr := baseClient.Get(ctx, volume.NamespacedName(), currentVolume); getErr != nil {
			return false, getErr
		}
		readyCondition := apimeta.FindStatusCondition(currentVolume.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerVolumeReasonCreateFailed), nil
	})
	require.NoError(t, waitErr)

	currentVolume := &apiv2.PhysicalContainerVolume{}
	require.NoError(t, baseClient.Get(ctx, volume.NamespacedName(), currentVolume))
	require.NoError(t, baseClient.Delete(ctx, currentVolume))

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		if getErr := baseClient.Get(ctx, volume.NamespacedName(), currentVolume); getErr != nil {
			return false, getErr
		}
		readyCondition := apimeta.FindStatusCondition(currentVolume.Status.Conditions, string(apiv2.ConditionReady))
		return readyCondition != nil &&
			readyCondition.Reason == string(apiv2.PhysicalContainerVolumeReasonRuntimeVolumeRemoveFailed), nil
	})
	require.NoError(t, waitErr)
	require.NotEmpty(t, currentVolume.Finalizers)

	inspectedVolumes, inspectErr := baseOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.NoError(t, inspectErr)
	require.Len(t, inspectedVolumes, 1)

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, reconcileErr := reconciler.Reconcile(ctx, request)
		if reconcileErr != nil {
			return false, reconcileErr
		}

		getErr := baseClient.Get(ctx, volume.NamespacedName(), currentVolume)
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, getErr
		}
		return len(currentVolume.Finalizers) == 0, nil
	})
	require.NoError(t, waitErr)

	_, inspectErr = baseOrchestrator.InspectVolumes(ctx, containers.InspectVolumesOptions{Volumes: []string{volumeName}})
	require.ErrorIs(t, inspectErr, containers.ErrNotFound)
}

func TestV2PhysicalContainerVolumeControllerRetainsCreatedVolumeUntilStatusIsDurable(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	namespace := durablePhysicalContainerVolumeNamespace("durable-created-volume")
	volume := durablePhysicalContainerVolume(namespace.Name, "durable-created-volume", "durable-created-volume-runtime")
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerVolume{}).
		WithObjects(namespace, volume).
		Build()
	statusClient := &failOncePhysicalContainerVolumeStatusClient{
		Client:  baseClient,
		failure: errors.New("simulated ready status save failure"),
		shouldFail: func(volume *apiv2.PhysicalContainerVolume) bool {
			return volume.Status.Phase == apiv2.PhysicalContainerVolumePhaseReady
		},
	}
	orchestrator := newDurabilityTestContainerOrchestrator(t, ctx)
	reconciler := controllers.NewPhysicalContainerVolumeReconciler(ctx, statusClient, baseClient, testutil.NewLogForTesting(t.Name()), orchestrator)
	request := ctrl.Request{NamespacedName: volume.NamespacedName()}

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
	require.Equal(t, 1, orchestrator.CreateVolumeCallCount(volume.Spec.Volume.VolumeName))

	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		_, currentReconcileErr := reconciler.Reconcile(ctx, request)
		if currentReconcileErr != nil {
			return false, currentReconcileErr
		}
		currentVolume := &apiv2.PhysicalContainerVolume{}
		if getErr := baseClient.Get(ctx, volume.NamespacedName(), currentVolume); getErr != nil {
			return false, getErr
		}
		return currentVolume.Status.Phase == apiv2.PhysicalContainerVolumePhaseReady, nil
	})
	require.NoError(t, waitErr)
	require.Equal(t, 1, orchestrator.CreateVolumeCallCount(volume.Spec.Volume.VolumeName))
}

func TestV2PhysicalContainerVolumeControllerRetainsTerminalFailureUntilStatusIsDurable(t *testing.T) {
	testCases := map[string]func(string) error{
		"conflict": func(name string) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: apiv2.GroupName, Resource: "physicalcontainervolumes"},
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
			namespace := durablePhysicalContainerVolumeNamespace("durable-volume-failure")
			volume := durablePhysicalContainerVolume(namespace.Name, "durable-volume-failure", "durable-volume-failure-runtime")
			baseClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&apiv2.Namespace{}, &apiv2.PhysicalContainerVolume{}).
				WithObjects(namespace, volume).
				Build()
			statusClient := &failOncePhysicalContainerVolumeStatusClient{
				Client:  baseClient,
				failure: makeFailure(volume.Name),
				shouldFail: func(volume *apiv2.PhysicalContainerVolume) bool {
					return volume.Status.Phase == apiv2.PhysicalContainerVolumePhaseFailed
				},
			}
			orchestrator := newDurabilityTestContainerOrchestrator(t, ctx)
			require.NoError(t, orchestrator.CreateVolume(ctx, containers.CreateVolumeOptions{Name: volume.Spec.Volume.VolumeName}))
			reconciler := controllers.NewPhysicalContainerVolumeReconciler(ctx, statusClient, baseClient, testutil.NewLogForTesting(t.Name()), orchestrator)
			request := ctrl.Request{NamespacedName: volume.NamespacedName()}

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
			require.Equal(t, 2, orchestrator.CreateVolumeCallCount(volume.Spec.Volume.VolumeName))

			waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
				_, currentReconcileErr := reconciler.Reconcile(ctx, request)
				if currentReconcileErr != nil {
					return false, currentReconcileErr
				}
				currentVolume := &apiv2.PhysicalContainerVolume{}
				if getErr := baseClient.Get(ctx, volume.NamespacedName(), currentVolume); getErr != nil {
					return false, getErr
				}
				return currentVolume.Status.Phase == apiv2.PhysicalContainerVolumePhaseFailed, nil
			})
			require.NoError(t, waitErr)
			require.Equal(t, 2, orchestrator.CreateVolumeCallCount(volume.Spec.Volume.VolumeName))
		})
	}
}

func durablePhysicalContainerVolumeNamespace(name string) *apiv2.Namespace {
	return &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Finalizers: []string{apiv2.NamespaceFinalizer},
		},
		Status: apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}
}

func durablePhysicalContainerVolume(namespace, name, volumeName string) *apiv2.PhysicalContainerVolume {
	return &apiv2.PhysicalContainerVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			UID:        types.UID(name),
			Finalizers: []string{apiv2.GroupName + "/physicalcontainervolume-reconciler"},
		},
		Spec: apiv2.PhysicalContainerVolumeSpec{
			Volume: &apiv2.PhysicalContainerVolumeConfig{VolumeName: volumeName},
		},
	}
}

func newDurabilityTestContainerOrchestrator(t *testing.T, ctx context.Context) *ctrl_testutil.TestContainerOrchestrator {
	t.Helper()
	orchestrator, orchestratorErr := ctrl_testutil.NewTestContainerOrchestrator(
		ctx,
		testutil.NewLogForTesting(t.Name()),
		ctrl_testutil.TcoOptionNone,
	)
	require.NoError(t, orchestratorErr)
	t.Cleanup(func() {
		require.NoError(t, orchestrator.Close())
	})
	return orchestrator
}
