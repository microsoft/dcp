/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientretry "k8s.io/client-go/util/retry"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type endpointDeletionFailureClient struct {
	ctrl_client.Client
	fail     atomic.Bool
	attempts atomic.Int32
}

type endpointStopFailureClient struct {
	*endpointDeletionFailureClient
	failStop        atomic.Bool
	stopAttempts    atomic.Int32
	failLookup      atomic.Bool
	failImageLookup atomic.Bool
	lookupAttempts  atomic.Int32
	lastObservation atomic.Int64
}

func (apiClient *endpointStopFailureClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	fail := false
	switch list.(type) {
	case *apiv2.PhysicalProcessList, *apiv2.PhysicalContainerList, *apiv2.PhysicalContainerNetworkList, *apiv2.PhysicalContainerExecList:
		apiClient.lastObservation.Store(time.Now().UnixNano())
		fail = apiClient.failLookup.Load()
	case *apiv2.PhysicalContainerImageList:
		apiClient.lastObservation.Store(time.Now().UnixNano())
		fail = apiClient.failImageLookup.Load()
	}
	if fail {
		apiClient.lookupAttempts.Add(1)
		return errors.New("physical backing lookup unavailable")
	}
	return apiClient.Client.List(ctx, list, options...)
}

func (apiClient *endpointStopFailureClient) Patch(
	ctx context.Context, object ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption,
) error {
	stopping := false
	switch physical := object.(type) {
	case *apiv2.PhysicalProcess:
		stopping = physical.Spec.Stop
	case *apiv2.PhysicalContainer:
		stopping = physical.Spec.Stop
	}
	if stopping {
		apiClient.stopAttempts.Add(1)
		if apiClient.failStop.Load() {
			return errors.New("physical stop request unavailable")
		}
	}
	return apiClient.Client.Patch(ctx, object, patch, options...)
}

// Verifies that Stop withdraws logical endpoints despite backing lookup or stop-patch failures,
// while endpoint deletion failures do not block physical Stop and withdrawal retries preserve the service.
// A failed container stop request preserves Running, whereas failed backing observation reports Unknown.
func TestV2LogicalEndpointsWithdrawBeforeStopCompletes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, failure := range []string{"stop", "endpoint", "lookup"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				var decorated *endpointStopFailureClient
				server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
					TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						decorated = &endpointStopFailureClient{endpointDeletionFailureClient: &endpointDeletionFailureClient{Client: apiClient}}
						return decorated
					}})
				require.NoError(t, startupErr)
				defer decorated.failStop.Store(false)
				defer decorated.fail.Store(false)
				defer decorated.failLookup.Store(false)
				apiClient := server.Client
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "endpoint-stop"}}
				require.NoError(t, apiClient.Create(ctx, namespace))
				workload := createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, kind)
				decorated.failStop.Store(failure == "stop")
				decorated.fail.Store(failure == "endpoint")
				decorated.failLookup.Store(failure == "lookup")
				require.NoError(t, apiClient.Patch(ctx, workload.logical,
					ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				if failure == "endpoint" {
					require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
						if getErr := apiClient.Get(pollCtx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing); getErr != nil {
							return false, getErr
						}
						if physical, isProcess := workload.backing.(*apiv2.PhysicalProcess); isProcess {
							return physical.Spec.Stop, nil
						}
						return workload.backing.(*apiv2.PhysicalContainer).Spec.Stop, nil
					}))
					require.Positive(t, decorated.attempts.Load())
					decorated.fail.Store(false)
				}
				service := waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current), nil
				})
				require.Equal(t, workload.service.UID, service.UID)
				require.Equal(t, workload.service.Status.EffectiveAddresses, service.Status.EffectiveAddresses)
				require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
					if failure == "lookup" {
						return decorated.lookupAttempts.Load() > 0, nil
					}
					return decorated.stopAttempts.Load() > 0, nil
				}))
				var endpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Empty(t, endpoints.Items)
				if failure == "stop" || failure == "lookup" {
					require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
					if physical, isProcess := workload.backing.(*apiv2.PhysicalProcess); isProcess {
						require.False(t, physical.Spec.Stop)
						require.Equal(t, apiv2.PhysicalProcessPhaseRunning, physical.Status.Phase)
						observed := waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(workload.logical), func(current *apiv2.LogicalProcess) (bool, error) {
							ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
							return ready != nil && ready.ObservedGeneration == current.Generation && current.Spec.Stop, nil
						})
						if failure == "stop" {
							require.Equal(t, apiv2.LogicalProcessPhaseRunning, observed.Status.Phase)
							requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalProcessReasonRunning)
						} else {
							require.Equal(t, apiv2.LogicalProcessPhaseUnknown, observed.Status.Phase)
							requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalProcessReasonLookupFailed)
						}
					} else {
						physical := workload.backing.(*apiv2.PhysicalContainer)
						require.False(t, physical.Spec.Stop)
						require.Equal(t, apiv2.PhysicalContainerPhaseRunning, physical.Status.Phase)
						observed := waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(workload.logical), func(current *apiv2.LogicalContainer) (bool, error) {
							ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
							return ready != nil && ready.ObservedGeneration == current.Generation && current.Spec.Stop, nil
						})
						if failure == "stop" {
							require.Equal(t, apiv2.LogicalContainerPhaseRunning, observed.Status.Phase)
							requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalContainerReasonRunning)
						} else {
							require.Equal(t, apiv2.LogicalContainerPhaseUnknown, observed.Status.Phase)
							requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerReasonLookupFailed)
						}
					}
				}
				decorated.failStop.Store(false)
				decorated.failLookup.Store(false)
				require.NoError(t, apiClient.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
			})
		}
	}
}

func (client *endpointDeletionFailureClient) Delete(
	ctx context.Context, object ctrl_client.Object, options ...ctrl_client.DeleteOption,
) error {
	if _, endpoint := object.(*apiv2.ServiceEndpoint); endpoint && client.fail.Load() {
		client.attempts.Add(1)
		return errors.New("endpoint deletion unavailable")
	}
	return client.Client.Delete(ctx, object, options...)
}

type endpointCleanupWorkload struct {
	logical ctrl_client.Object
	backing ctrl_client.Object
	service *apiv2.LogicalService
}

// Verifies that an exited logical process withdraws endpoints without forwarding a later Stop,
// reopening its lifecycle, or polling after terminal observation failures.
func TestV2LogicalProcessTerminalHandlerIgnoresLaterStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	var decorated *endpointStopFailureClient
	server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			decorated = &endpointStopFailureClient{endpointDeletionFailureClient: &endpointDeletionFailureClient{Client: apiClient}}
			return decorated
		}})
	require.NoError(t, startupErr)
	apiClient := server.Client
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "terminal-process-handler"}}
	require.NoError(t, apiClient.Create(ctx, namespace))
	workload := createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, "process")
	logical := workload.logical.(*apiv2.LogicalProcess)
	pid, pidErr := process.Int64_ToPidT(*logical.Status.PID)
	require.NoError(t, pidErr)
	environment.TestProcessExecutor.SimulateProcessExit(t, pid, 17)
	exited := waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
	})
	requireReadyCondition(t, exited.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalProcessReasonCompleted)
	require.Contains(t, apimeta.FindStatusCondition(exited.Status.Conditions, string(apiv2.ConditionReady)).Message, "17")
	waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current), nil
	})
	require.NoError(t, apiClient.Patch(ctx, logical,
		ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	observed := waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalProcess) (bool, error) {
		return current.Spec.Stop && current.Status.Phase == apiv2.LogicalProcessPhaseExited && len(current.Status.Conditions) > 0 &&
			current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
	})
	require.Equal(t, exited.Status.PID, observed.Status.PID)
	require.Equal(t, exited.Status.ExitCode, observed.Status.ExitCode)
	require.Zero(t, decorated.stopAttempts.Load())
	require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
	require.False(t, workload.backing.(*apiv2.PhysicalProcess).Spec.Stop)
	expectedStatus := observed.DeepCopy().Status
	decorated.failLookup.Store(true)
	defer decorated.failLookup.Store(false)
	reconcileTerminalObservationAndWaitForIdle(t, ctx, apiClient, decorated, observed)
	require.Equal(t, expectedStatus, observed.Status)
	lookupAttempts := decorated.lookupAttempts.Load()
	require.NoError(t, apiClient.Delete(ctx, observed))
	require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(context.Context) (bool, error) {
		return decorated.lookupAttempts.Load() > lookupAttempts, nil
	}))
	require.NoError(t, apiClient.Get(ctx, observed.NamespacedName(), observed))
	require.Equal(t, expectedStatus, observed.Status)
	decorated.failLookup.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, observed)
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
}

// Verifies that terminal logical containers preserve their lifecycle after later Stop requests,
// failed backing/image reads, or image identity changes, without polling except for cleanup.
func TestV2LogicalContainerTerminalHandlerIgnoresLaterStop(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"exit", "failed", "image-read", "image-identity", "preparation", "startup-lease"} {
		t.Run(terminal, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			var decorated *endpointStopFailureClient
			var ownershipReader *namespaceWorkloadFailureReader
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						decorated = &endpointStopFailureClient{endpointDeletionFailureClient: &endpointDeletionFailureClient{Client: apiClient}}
						return decorated
					},
					DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
						ownershipReader = &namespaceWorkloadFailureReader{Reader: reader, namespaceName: "terminal-handler-" + terminal}
						return ownershipReader
					},
				})
			require.NoError(t, startupErr)
			apiClient := server.Client
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "terminal-handler-" + terminal}}
			require.NoError(t, apiClient.Create(ctx, namespace))
			var workload endpointCleanupWorkload
			var logical *apiv2.LogicalContainer
			expectedPhase := apiv2.LogicalContainerPhaseExited
			if terminal == "preparation" {
				expectedPhase = apiv2.LogicalContainerPhaseFailed
				logical = &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name},
					Spec: apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{
						Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name}, Command: []string{"{{"},
					}}}
				require.NoError(t, apiClient.Create(ctx, logical))
			} else if terminal == "startup-lease" {
				ownershipReader.fail.Store(true)
				defer ownershipReader.fail.Store(false)
				logical = &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: namespace.Name},
					Spec: apiv2.LogicalContainerSpec{Mode: apiv2.LogicalContainerModePersistent, ContainerName: namespace.Name,
						Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name}}}}
				require.NoError(t, apiClient.Create(ctx, logical))
				logical = waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
					return current.Status.ContainerID != "" && current.Status.Phase == apiv2.LogicalContainerPhaseRunning &&
						ready != nil && strings.Contains(ready.Message, "namespace workload lookup unavailable"), nil
				})
				requireReadyCondition(t, logical.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalContainerReasonRunning)
				require.NoError(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner))
			} else {
				workload = createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, "container")
				logical = workload.logical.(*apiv2.LogicalContainer)
			}
			if terminal == "failed" {
				expectedPhase = apiv2.LogicalContainerPhaseFailed
				_, removeErr := server.ContainerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
					Containers: []string{logical.Status.ContainerID}, Force: true,
				})
				require.NoError(t, removeErr)
			} else if terminal != "preparation" {
				orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				require.True(t, simulated)
				require.NoError(t, orchestrator.SimulateContainerExit(ctx, logical.Status.ContainerID, 17))
			}
			terminalContainer := waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == expectedPhase, nil
			})
			if expectedPhase == apiv2.LogicalContainerPhaseExited {
				requireReadyCondition(t, terminalContainer.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerReasonCompleted)
				require.Contains(t, apimeta.FindStatusCondition(terminalContainer.Status.Conditions, string(apiv2.ConditionReady)).Message, "17")
			}
			if workload.service != nil {
				waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current), nil
				})
			}
			require.NoError(t, apiClient.Patch(ctx, logical,
				ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			observed := waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Spec.Stop && current.Status.Phase == expectedPhase && len(current.Status.Conditions) > 0 &&
					current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
			})
			if terminal == "startup-lease" {
				require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, logical, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
			}
			require.NotEqual(t, string(apiv2.LogicalContainerReasonStopped), observed.Status.Conditions[0].Reason)
			require.Zero(t, decorated.stopAttempts.Load())
			if workload.backing != nil {
				require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
				require.False(t, workload.backing.(*apiv2.PhysicalContainer).Spec.Stop)
			}
			expectedStatus := observed.DeepCopy().Status
			switch terminal {
			case "image-read", "preparation":
				decorated.failImageLookup.Store(true)
			case "image-identity":
				image := &apiv2.PhysicalContainerImage{}
				require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: observed.Status.ImageRef}, image))
				image.Finalizers = slices.DeleteFunc(image.Finalizers, func(finalizer string) bool { return finalizer == controllers.LogicalBackingFinalizer })
				require.NoError(t, apiClient.Update(ctx, image))
			default:
				decorated.failLookup.Store(true)
			}
			defer decorated.failLookup.Store(false)
			defer decorated.failImageLookup.Store(false)
			reconcileTerminalObservationAndWaitForIdle(t, ctx, apiClient, decorated, observed)
			require.Equal(t, expectedStatus, observed.Status)
			decorated.failLookup.Store(false)
			decorated.failImageLookup.Store(false)
			require.NoError(t, apiClient.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
		})
	}
}

func reconcileTerminalObservationAndWaitForIdle(
	t *testing.T, ctx context.Context, apiClient ctrl_client.Client, decorated *endpointStopFailureClient, logical ctrl_client.Object,
) {
	t.Helper()
	previousRead, previousAttempts := decorated.lastObservation.Load(), decorated.lookupAttempts.Load()
	require.NoError(t, apiClient.Patch(ctx, logical, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"terminal-check":"true"}}}`))))
	require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, false, func(context.Context) (bool, error) {
		failureObserved := !decorated.failLookup.Load() && !decorated.failImageLookup.Load() ||
			decorated.lookupAttempts.Load() > previousAttempts
		return decorated.lastObservation.Load() > previousRead && failureObserved, nil
	}))
	waitControllerIdle(t, ctx, &decorated.lastObservation)
	require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(logical), logical))
}

func createEndpointCleanupWorkload(
	t *testing.T, ctx context.Context, apiClient ctrl_client.Client, namespace string, kind string,
) endpointCleanupWorkload {
	t.Helper()
	name := types.NamespacedName{Name: "workload", Namespace: namespace}
	binding := apiv2.LogicalServiceBinding{Name: "http", Service: &apiv2.LogicalServiceTemplate{
		Name: "http", LogicalServiceSpec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"},
	}}
	var workload endpointCleanupWorkload
	if kind == "process" {
		logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
			Spec: apiv2.LogicalProcessSpec{
				Process: &apiv2.LogicalProcessConfig{ExecutablePath: "endpoint-cleanup-process"},
				Ports:   []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Port: 8080}},
			}}
		require.NoError(t, apiClient.Create(ctx, logical))
		running := waitObjectAssumesStateEx(t, ctx, apiClient, name, func(current *apiv2.LogicalProcess) (bool, error) {
			return current.Status.Phase == apiv2.LogicalProcessPhaseRunning, nil
		})
		workload.logical = running
		workload.backing = &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalProcessRef, Namespace: namespace}}
	} else {
		logical := &apiv2.LogicalContainer{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
			Spec: apiv2.LogicalContainerSpec{
				Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "endpoint-cleanup-image"}},
				Ports:     []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}},
			}}
		require.NoError(t, apiClient.Create(ctx, logical))
		running := waitObjectAssumesStateEx(t, ctx, apiClient, name, func(current *apiv2.LogicalContainer) (bool, error) {
			return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
		})
		workload.logical = running
		workload.backing = &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: running.Status.PhysicalContainerRef, Namespace: namespace}}
	}
	require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
	workload.service = waitObjectAssumesStateEx(t, ctx, apiClient, types.NamespacedName{Name: "workload-http", Namespace: namespace},
		func(current *apiv2.LogicalService) (bool, error) {
			return logicalServiceReady(current), nil
		})
	return workload
}

// Verifies that endpoint deletion errors preserve Running readiness or normal deletion progress,
// while cleanup removes obsolete owned bindings, preserves unrelated endpoints, and retries independently of physical cleanup.
func TestV2LogicalEndpointDeletionDoesNotWaitForBackingCleanup(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, failEndpoints := range []bool{false, true} {
			name := kind + "/available"
			if failEndpoints {
				name = kind + "/endpoint-deletion-failed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				var deletionClient *endpointDeletionFailureClient
				server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
					TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						deletionClient = &endpointDeletionFailureClient{Client: apiClient}
						return deletionClient
					}})
				require.NoError(t, startupErr)
				defer deletionClient.fail.Store(false)
				apiClient := server.Client
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "endpoint-deletion"}}
				require.NoError(t, apiClient.Create(ctx, namespace))
				workload := createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, kind)
				var initialEndpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &initialEndpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Len(t, initialEndpoints.Items, 1)
				if !failEndpoints {
					priorAttempts := deletionClient.attempts.Load()
					deletionClient.fail.Store(true)
					obsolete := &apiv2.ServiceEndpoint{
						ObjectMeta: metav1.ObjectMeta{Name: "obsolete", Namespace: namespace.Name,
							OwnerReferences: initialEndpoints.Items[0].OwnerReferences},
						Spec: initialEndpoints.Items[0].Spec,
					}
					require.NoError(t, apiClient.Create(ctx, obsolete))
					require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
						return deletionClient.attempts.Load() >= priorAttempts+2, nil
					}))
					require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.logical), workload.logical))
					if logicalProcess, isProcess := workload.logical.(*apiv2.LogicalProcess); isProcess {
						require.Equal(t, apiv2.LogicalProcessPhaseRunning, logicalProcess.Status.Phase)
						requireReadyCondition(t, logicalProcess.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalProcessReasonRunning)
					} else {
						logicalContainer := workload.logical.(*apiv2.LogicalContainer)
						require.Equal(t, apiv2.LogicalContainerPhaseRunning, logicalContainer.Status.Phase)
						requireReadyCondition(t, logicalContainer.Status.Conditions, metav1.ConditionTrue, apiv2.LogicalContainerReasonRunning)
					}
					deletionClient.fail.Store(false)
					ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, obsolete)
				}
				unrelated := &apiv2.ServiceEndpoint{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
					Spec: apiv2.ServiceEndpointSpec{ServiceRef: "other-service",
						Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 9000}}},
				}
				require.NoError(t, apiClient.Create(ctx, unrelated))
				const backingHold = "tests.dcp.microsoft.com/endpoint-cleanup-hold"
				workload.backing.SetFinalizers(append(workload.backing.GetFinalizers(), backingHold))
				require.NoError(t, apiClient.Update(ctx, workload.backing))
				deletionClient.fail.Store(failEndpoints)
				require.NoError(t, apiClient.Delete(ctx, workload.logical))
				require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
					getErr := apiClient.Get(pollCtx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing)
					return getErr == nil && !workload.backing.GetDeletionTimestamp().IsZero() &&
						!slices.Contains(workload.backing.GetFinalizers(), apiv2.GroupName+"/physical"+kind+"-reconciler"), getErr
				}))
				if failEndpoints {
					require.Positive(t, deletionClient.attempts.Load())
					logicalName := ctrl_client.ObjectKeyFromObject(workload.logical)
					if _, isProcess := workload.logical.(*apiv2.LogicalProcess); isProcess {
						pending := waitObjectAssumesStateEx(t, ctx, apiClient, logicalName, func(current *apiv2.LogicalProcess) (bool, error) {
							return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
						})
						requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalProcessReasonStopped)
					} else {
						pending := waitObjectAssumesStateEx(t, ctx, apiClient, logicalName, func(current *apiv2.LogicalContainer) (bool, error) {
							return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
						})
						requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.LogicalContainerReasonStopped)
					}
					var pending apiv2.ServiceEndpointList
					require.NoError(t, apiClient.List(ctx, &pending, ctrl_client.InNamespace(namespace.Name)))
					pending.Items = slices.DeleteFunc(pending.Items, func(endpoint apiv2.ServiceEndpoint) bool {
						return !metav1.IsControlledBy(&endpoint, workload.logical)
					})
					require.Len(t, pending.Items, 1)
					require.True(t, pending.Items[0].DeletionTimestamp.IsZero())
					deletionClient.fail.Store(false)
				}
				waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current), nil
				})
				var endpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Len(t, endpoints.Items, 1)
				require.Equal(t, unrelated.UID, endpoints.Items[0].UID)
				require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.logical), workload.logical))
				require.False(t, workload.logical.GetDeletionTimestamp().IsZero())
				require.NoError(t, clientretry.RetryOnConflict(clientretry.DefaultRetry, func() error {
					if getErr := apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing); getErr != nil {
						return getErr
					}
					workload.backing.SetFinalizers(slices.DeleteFunc(workload.backing.GetFinalizers(), func(finalizer string) bool {
						return finalizer == backingHold
					}))
					return apiClient.Update(ctx, workload.backing)
				}))
				require.NoError(t, apiClient.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
			})
		}
	}
}

// Verifies that normal exit, missing runtime or process backing, and invalid logical backing holds
// withdraw endpoints without deleting their services, including retry after an endpoint deletion failure.
func TestV2LogicalEndpointsWithdrawOnTerminalState(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, terminal := range []string{"exit", "invalid", "missing", "terminating"} {
			t.Run(kind+"/"+terminal, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				defer cancel()
				var deletionClient *endpointDeletionFailureClient
				server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx, logicalServicePreparationControllers, t.Name(), t.TempDir(),
					TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						deletionClient = &endpointDeletionFailureClient{Client: apiClient}
						return deletionClient
					}})
				require.NoError(t, startupErr)
				defer deletionClient.fail.Store(false)
				apiClient := server.Client
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "terminal-endpoints"}}
				require.NoError(t, apiClient.Create(ctx, namespace))
				workload := createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, kind)
				deletionClient.fail.Store(terminal == "invalid")
				switch terminal {
				case "exit":
					if logical, isProcess := workload.logical.(*apiv2.LogicalProcess); isProcess {
						pid, pidErr := process.Int64_ToPidT(*logical.Status.PID)
						require.NoError(t, pidErr)
						environment.TestProcessExecutor.SimulateProcessExit(t, pid, 17)
					} else {
						containerLogical := workload.logical.(*apiv2.LogicalContainer)
						orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
						require.True(t, simulated)
						require.NoError(t, orchestrator.SimulateContainerStatus(ctx, containerLogical.Status.ContainerID, containers.ContainerStatusExited))
					}
				case "invalid":
					require.NoError(t, clientretry.RetryOnConflict(clientretry.DefaultRetry, func() error {
						if getErr := apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing); getErr != nil {
							return getErr
						}
						workload.backing.SetFinalizers(slices.DeleteFunc(workload.backing.GetFinalizers(), func(finalizer string) bool {
							return finalizer == controllers.LogicalBackingFinalizer
						}))
						return apiClient.Update(ctx, workload.backing)
					}))
				case "missing":
					if physicalProcess, isProcess := workload.backing.(*apiv2.PhysicalProcess); isProcess {
						forceRemoveLogicalProcessBacking(t, ctx, apiClient, physicalProcess)
					} else {
						containerLogical := workload.logical.(*apiv2.LogicalContainer)
						_, removeErr := server.ContainerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
							Containers: []string{containerLogical.Status.ContainerID}, Force: true,
						})
						require.NoError(t, removeErr)
					}
				case "terminating":
					workload.backing.SetFinalizers(append(workload.backing.GetFinalizers(), "tests.dcp.microsoft.com/hold"))
					require.NoError(t, apiClient.Update(ctx, workload.backing))
					require.NoError(t, apiClient.Delete(ctx, workload.backing))
				}
				if terminal == "terminating" {
					require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
					require.False(t, workload.backing.GetDeletionTimestamp().IsZero())
				} else if kind == "process" {
					waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(workload.logical), func(current *apiv2.LogicalProcess) (bool, error) {
						if terminal == "exit" {
							return current.Status.Phase == apiv2.LogicalProcessPhaseExited, nil
						}
						return current.Status.Phase == apiv2.LogicalProcessPhaseFailed, nil
					})
				} else {
					waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(workload.logical), func(current *apiv2.LogicalContainer) (bool, error) {
						if terminal == "exit" {
							return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
						}
						return current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
					})
				}
				if terminal == "invalid" {
					require.Positive(t, deletionClient.attempts.Load())
					deletionClient.fail.Store(false)
				}
				service := waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhaseRunning && !logicalServiceReady(current), nil
				})
				require.Equal(t, workload.service.UID, service.UID)
				require.Equal(t, workload.service.Status.EffectiveAddresses, service.Status.EffectiveAddresses)
				var endpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Empty(t, endpoints.Items)
				require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.logical), workload.logical))
				require.True(t, workload.logical.GetDeletionTimestamp().IsZero())
				if terminal == "terminating" {
					require.NoError(t, clientretry.RetryOnConflict(clientretry.DefaultRetry, func() error {
						if getErr := apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing); getErr != nil {
							return getErr
						}
						workload.backing.SetFinalizers(slices.DeleteFunc(workload.backing.GetFinalizers(), func(finalizer string) bool {
							return finalizer == "tests.dcp.microsoft.com/hold"
						}))
						return apiClient.Update(ctx, workload.backing)
					}))
				}
				require.NoError(t, apiClient.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
				if terminal == "missing" {
					require.True(t, apierrors.IsNotFound(apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing)))
				}
			})
		}
	}
}
