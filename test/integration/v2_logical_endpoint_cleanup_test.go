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

// Verifies that logical deletion withdraws endpoints before backing deletion completes,
// and endpoint deletion failures cannot block backing cleanup or prevent later withdrawal.
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
					require.Len(t, pending.Items, 1)
					require.True(t, pending.Items[0].DeletionTimestamp.IsZero())
					deletionClient.fail.Store(false)
				}
				waitObjectAssumesStateEx(t, ctx, apiClient, workload.service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
					return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
				})
				var endpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Empty(t, endpoints.Items)
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
		for _, terminal := range []string{"exit", "invalid", "missing"} {
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
				}
				if kind == "process" {
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
					return current.Status.Phase == apiv2.LogicalServicePhasePending, nil
				})
				require.Equal(t, workload.service.UID, service.UID)
				require.Equal(t, workload.service.Status.EffectiveAddresses, service.Status.EffectiveAddresses)
				var endpoints apiv2.ServiceEndpointList
				require.NoError(t, apiClient.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
				require.Empty(t, endpoints.Items)
				require.NoError(t, apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.logical), workload.logical))
				require.True(t, workload.logical.GetDeletionTimestamp().IsZero())
				require.NoError(t, apiClient.Delete(ctx, namespace))
				ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, namespace)
				if terminal == "missing" {
					require.True(t, apierrors.IsNotFound(apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(workload.backing), workload.backing)))
				}
			})
		}
	}
}
