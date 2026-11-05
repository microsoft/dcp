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
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientretry "k8s.io/client-go/util/retry"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
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
	failStop     atomic.Bool
	stopAttempts atomic.Int32
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

// Verifies that Stop withdraws logical endpoints even when forwarding Stop fails, while endpoint
// deletion failures do not block physical Stop and withdrawal retries without deleting the service.
func TestV2LogicalEndpointsWithdrawBeforeStopCompletes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"process", "container"} {
		for _, failure := range []string{"stop", "endpoint"} {
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
				apiClient := server.Client
				namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "endpoint-stop"}}
				require.NoError(t, apiClient.Create(ctx, namespace))
				workload := createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, kind)
				decorated.failStop.Store(failure == "stop")
				decorated.fail.Store(failure == "endpoint")
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
					return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
				})
				require.Equal(t, workload.service.UID, service.UID)
				require.Equal(t, workload.service.Status.EffectiveAddresses, service.Status.EffectiveAddresses)
				require.NoError(t, wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
					return decorated.stopAttempts.Load() > 0, nil
				}))
				var endpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Empty(t, endpoints.Items)
				if failure == "stop" {
					require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
					if physical, isProcess := workload.backing.(*apiv2.PhysicalProcess); isProcess {
						require.False(t, physical.Spec.Stop)
						require.Equal(t, apiv2.PhysicalProcessPhaseRunning, physical.Status.Phase)
					} else {
						physical := workload.backing.(*apiv2.PhysicalContainer)
						require.False(t, physical.Spec.Stop)
						require.Equal(t, apiv2.PhysicalContainerPhaseRunning, physical.Status.Phase)
					}
				}
				decorated.failStop.Store(false)
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

// Verifies that an exited logical process withdraws its endpoints and refreshes final observations
// without forwarding a later Stop request or reopening process stopping.
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
	waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
		return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
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
	require.NoError(t, apiClient.Delete(ctx, namespace))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
}

// Verifies that terminal logical containers withdraw endpoints and refresh observations without
// forwarding a later Stop request or reopening their lifecycle as Stopping.
func TestV2LogicalContainerTerminalHandlerIgnoresLaterStop(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"exit", "failed"} {
		t.Run(terminal, func(t *testing.T) {
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
			apiClient := server.Client
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "terminal-handler-" + terminal}}
			require.NoError(t, apiClient.Create(ctx, namespace))
			workload := createEndpointCleanupWorkload(t, ctx, apiClient, namespace.Name, "container")
			logical := workload.logical.(*apiv2.LogicalContainer)
			expectedPhase := apiv2.LogicalContainerPhaseExited
			if terminal == "exit" {
				orchestrator, simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				require.True(t, simulated)
				require.NoError(t, orchestrator.SimulateContainerStatus(ctx, logical.Status.ContainerID, containers.ContainerStatusExited))
			} else {
				expectedPhase = apiv2.LogicalContainerPhaseFailed
				_, removeErr := server.ContainerOrchestrator.RemoveContainers(ctx, containers.RemoveContainersOptions{
					Containers: []string{logical.Status.ContainerID}, Force: true,
				})
				require.NoError(t, removeErr)
			}
			waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Status.Phase == expectedPhase, nil
			})
			waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
			})
			require.NoError(t, apiClient.Patch(ctx, logical,
				ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			observed := waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return current.Spec.Stop && current.Status.Phase == expectedPhase && len(current.Status.Conditions) > 0 &&
					current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
			})
			require.NotEqual(t, "ContainerStopped", observed.Status.Conditions[0].Reason)
			require.Zero(t, decorated.stopAttempts.Load())
			require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing))
			require.False(t, workload.backing.(*apiv2.PhysicalContainer).Spec.Stop)
			require.NoError(t, apiClient.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
		})
	}
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
			return current.Status.Phase == apiv2.LogicalServicePhaseReady, nil
		})
	return workload
}

// Verifies that endpoint synchronization removes obsolete owned bindings but preserves unrelated
// endpoints, and logical deletion withdraws endpoints independently of backing cleanup or endpoint errors.
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
					obsolete := &apiv2.ServiceEndpoint{
						ObjectMeta: metav1.ObjectMeta{Name: "obsolete", Namespace: namespace.Name,
							OwnerReferences: initialEndpoints.Items[0].OwnerReferences},
						Spec: initialEndpoints.Items[0].Spec,
					}
					require.NoError(t, apiClient.Create(ctx, obsolete))
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
					return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
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
						if terminal == "invalid" {
							return current.Status.Phase == apiv2.LogicalProcessPhaseUnknown, nil
						}
						return current.Status.Phase == apiv2.LogicalProcessPhaseFailed, nil
					})
				} else {
					waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(workload.logical), func(current *apiv2.LogicalContainer) (bool, error) {
						if terminal == "exit" {
							return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
						}
						if terminal == "invalid" {
							return current.Status.Phase == apiv2.LogicalContainerPhaseUnknown, nil
						}
						return current.Status.Phase == apiv2.LogicalContainerPhaseFailed, nil
					})
				}
				if terminal == "invalid" {
					require.Positive(t, deletionClient.attempts.Load())
					deletionClient.fail.Store(false)
				}
				service := waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
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
