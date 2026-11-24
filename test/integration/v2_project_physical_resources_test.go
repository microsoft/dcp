/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
)

type projectPhysicalClient struct {
	ctrl_client.Client
	kind                    string
	blockFinalStatus        atomic.Bool
	finalStatus             chan ctrl_client.Object
	finalStatusOnce         sync.Once
	loseHoldResponse        atomic.Bool
	holdResponseLost        atomic.Bool
	hidePhysicalResources   atomic.Bool
	undecodedCreates        atomic.Int32
	failNetwork             atomic.Bool
	networkLookups          atomic.Int32
	failContainerLists      atomic.Bool
	containerListFailures   atomic.Int32
	staleContainerCondition atomic.Bool
	reportContainerRunning  atomic.Bool
	reportContainerUnknown  atomic.Bool
	reportContainerPending  atomic.Bool
	failNamespace           atomic.Bool
	namespaceLookups        atomic.Int32
	onUndecodedCreate       func(ctrl_client.Object)
}

// Verifies that a Failed ProjectContainer preserves its original readable capture on ambiguous discovery
// or logical-hold loss, including existing followers and clients opening logs after the transition.
// Later capture completion is observed without reopening the terminal lifecycle.
func TestV2ProjectContainerPhysicalInterferencePreservesRuntimeLogs(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"ambiguous", "hold-removed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-logs-"+scenario)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
				}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, client.Create(ctx, resource))
			prepared := startProjectContainerAdapter(t, ctx, client, containerOrchestrator, waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady))
			require.NoError(t, containerOrchestrator.SimulateContainerLogging(prepared.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("before-unknown\n")))
			follow, followErr := restClient.Get().
				AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projectcontainers", resource.Name, "log").
				Param("follow", "true").Stream(ctx)
			require.NoError(t, followErr)
			defer follow.Close()
			first := make([]byte, len("before-unknown\n"))
			_, firstErr := io.ReadFull(follow, first)
			require.NoError(t, firstErr)
			require.Equal(t, "before-unknown\n", string(first))
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
			var duplicate *apiv2.PhysicalContainer
			if scenario == "ambiguous" {
				duplicate = physical.DeepCopy()
				duplicate.ObjectMeta = metav1.ObjectMeta{Name: "duplicate", Namespace: namespace.Name,
					OwnerReferences: physical.OwnerReferences, Finalizers: physical.Finalizers}
				require.NoError(t, client.Create(ctx, duplicate))
				cleanupProjectPhysical(t, duplicate)
			} else {
				removeProjectPhysicalHold(t, ctx, client, physical)
			}
			unknown := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, unknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
			require.NoError(t, client.Get(ctx, physical.NamespacedName(), physical))
			require.Equal(t, physical.GetLogSource(), unknown.Status.RuntimeLogs)
			require.True(t, unknown.Status.RuntimeLogs.Available)
			require.False(t, unknown.Status.RuntimeLogs.Complete)
			require.NoError(t, containerOrchestrator.SimulateContainerLogging(prepared.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("after-unknown\n")))
			next := make([]byte, len("after-unknown\n"))
			_, nextErr := io.ReadFull(follow, next)
			require.NoError(t, nextErr)
			require.Equal(t, "after-unknown\n", string(next))
			late, lateErr := restClient.Get().
				AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projectcontainers", resource.Name, "log").
				Stream(ctx)
			require.NoError(t, lateErr)
			defer late.Close()
			output, readErr := io.ReadAll(late)
			require.NoError(t, readErr)
			require.Equal(t, "before-unknown\nafter-unknown\n", string(output))
			if duplicate != nil {
				require.NoError(t, client.Delete(ctx, duplicate))
				removeProjectPhysicalHold(t, ctx, client, duplicate)
				ctrl_testutil.WaitObjectDeleted(t, ctx, client, duplicate)
				require.NoError(t, containerOrchestrator.SimulateContainerStatus(ctx, prepared.Status.ContainerID, containers.ContainerStatusDead))
				completed := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseFailed && current.Status.RuntimeLogs != nil &&
						current.Status.RuntimeLogs.Available && current.Status.RuntimeLogs.Complete, nil
				})
				require.Equal(t, unknown.Status.Conditions[0].Reason, completed.Status.Conditions[0].Reason)
				require.Equal(t, unknown.Status.Conditions[0].Message, completed.Status.Conditions[0].Message)
				require.Equal(t, prepared.Status.RuntimeLogs.UID, completed.Status.RuntimeLogs.UID)
				remaining, drainErr := io.ReadAll(follow)
				require.NoError(t, drainErr)
				require.Empty(t, remaining)
			}
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		})
	}
}

func (apiClient *projectPhysicalClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "ProjectContainer" || !apiClient.hidePhysicalResources.Load() {
		return apiClient.Client.Create(ctx, obj, options...)
	}
	committed := obj.DeepCopyObject().(ctrl_client.Object)
	if createErr := apiClient.Client.Create(ctx, committed, options...); createErr != nil {
		return createErr
	}
	apiClient.undecodedCreates.Add(1)
	if apiClient.onUndecodedCreate != nil {
		apiClient.onUndecodedCreate(committed)
	}
	return errors.New("Project physical resource create committed without a decoded response")
}

func (apiClient *projectPhysicalClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, containers := list.(*apiv2.PhysicalContainerList); containers && apiClient.failContainerLists.Load() {
		apiClient.containerListFailures.Add(1)
		return errors.New("injected Project container lookup failure")
	}
	if listErr := apiClient.Client.List(ctx, list, options...); listErr != nil {
		return listErr
	}
	if containers, valid := list.(*apiv2.PhysicalContainerList); valid {
		for index := range containers.Items {
			if apiClient.reportContainerRunning.Load() {
				containers.Items[index].Status.Phase = apiv2.PhysicalContainerPhaseRunning
			}
			if apiClient.reportContainerUnknown.Load() {
				containers.Items[index].Status.Phase = apiv2.PhysicalContainerPhaseUnknown
			}
			if apiClient.reportContainerPending.Load() {
				containers.Items[index].Status.Phase = apiv2.PhysicalContainerPhasePending
			}
			if apiClient.staleContainerCondition.Load() {
				containers.Items[index].Generation++
			}
			for conditionIndex := range containers.Items[index].Status.Conditions {
				if apiClient.reportContainerPending.Load() {
					containers.Items[index].Status.Conditions[conditionIndex].Message = "injected pending physical resource observation"
				}
				if apiClient.staleContainerCondition.Load() {
					containers.Items[index].Status.Conditions[conditionIndex].ObservedGeneration = containers.Items[index].Generation - 1
				}
				if apiClient.reportContainerRunning.Load() {
					containers.Items[index].Status.Conditions[conditionIndex].Status = metav1.ConditionTrue
				}
			}
		}
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if !apiClient.hidePhysicalResources.Load() || listOptions.FieldSelector == nil ||
		!strings.Contains(listOptions.FieldSelector.String(), ".metadata.projectContainerOwnerUID") {
		return nil
	}
	switch children := list.(type) {
	case *apiv2.PhysicalProcessList:
		children.Items = nil
	case *apiv2.PhysicalContainerList:
		children.Items = nil
	case *apiv2.PhysicalContainerImageList:
		children.Items = nil
	}
	return nil
}

// Verifies that ProjectContainer retains its terminal Failed state and original diagnostic after physical resource
// disappearance or identity replacement, while still completing missing log sources.
func TestV2ProjectContainerFailureSurvivesPhysicalChanges(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"missing", "replaced"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-failed-"+scenario)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
				}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, client.Create(ctx, resource))
			prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			removeProjectRuntimeContainer(t, ctx, containerOrchestrator, prepared.Status.ContainerID)
			failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, client.Get(ctx, types.NamespacedName{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
			replacement := physical.DeepCopy()
			replacement.ObjectMeta = metav1.ObjectMeta{Name: physical.Name, Namespace: namespace.Name,
				OwnerReferences: physical.OwnerReferences, Finalizers: physical.Finalizers}
			require.NoError(t, client.Delete(ctx, physical))
			removeProjectPhysicalHold(t, ctx, client, physical)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
			if scenario == "replaced" {
				require.NoError(t, client.Create(ctx, replacement))
				cleanupProjectPhysical(t, replacement)
				require.NotEqual(t, physical.UID, replacement.UID)
			}
			require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			observed := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				if scenario == "missing" && (current.Status.RuntimeLogs == nil ||
					!current.Status.RuntimeLogs.Complete || current.Status.RuntimeLogs.Available) {
					return false, nil
				}
				return current.Generation == resource.Generation && len(current.Status.Conditions) != 0 &&
					current.Status.Conditions[0].ObservedGeneration == resource.Generation, nil
			})
			require.Equal(t, apiv2.ProjectPhaseFailed, observed.Status.Phase)
			require.Equal(t, failed.Status.Conditions[0].Reason, observed.Status.Conditions[0].Reason)
			require.Equal(t, failed.Status.Conditions[0].Message, observed.Status.Conditions[0].Message)
			require.Equal(t, prepared.Status.RuntimeLogs.UID, observed.Status.RuntimeLogs.UID)
			require.Equal(t, prepared.Status.RuntimeLogs.Name, observed.Status.RuntimeLogs.Name)
			if scenario == "missing" {
				require.True(t, observed.Status.RuntimeLogs.Complete)
				require.False(t, observed.Status.RuntimeLogs.Available)
			}
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
			var physicalResources apiv2.PhysicalContainerList
			require.NoError(t, client.List(ctx, &physicalResources, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, physicalResources.Items)
		})
	}
}

// Verifies that a direct-mode ProjectContainer that fails while its application is still running keeps its
// failure diagnosis and still reports the application's later finish time and exit code.
func TestV2ProjectContainerFailedReportsLaterCompletion(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createV2ExecNamespace(t, ctx, client, "v2-project-failed-completion")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect},
			Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Entrypoint: "application",
			}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, resource))
	running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseActive)
	var images apiv2.PhysicalContainerImageList
	require.NoError(t, client.List(ctx, &images, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, images.Items, 1)
	// Losing the hold on the owned image fails the Project but leaves the trusted container running.
	removeProjectPhysicalHold(t, ctx, client, &images.Items[0])
	failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
	require.Nil(t, failed.Status.ExitCode)

	require.NoError(t, containerOrchestrator.SimulateContainerExit(ctx, running.Status.ContainerID, 9))
	completed := waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.ExitCode != nil, nil
	})
	require.Equal(t, int32(9), *completed.Status.ExitCode)
	require.False(t, completed.Status.FinishedAt.IsZero())
	require.Equal(t, apiv2.ProjectPhaseFailed, completed.Status.Phase)
	require.Equal(t, failed.Status.Conditions[0].Message, completed.Status.Conditions[0].Message)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
}

// Verifies that ProjectProcess keeps namespace lookup failures in Pending/Preparing with diagnostic context
// and becomes prepared after namespace observation recovers.
func TestV2ProjectProcessNamespaceLookupRetainsPreparation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectPhysicalClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			decorated = &projectPhysicalClient{Client: apiClient}
			return decorated
		}})
	require.NoError(t, startupErr)
	t.Cleanup(func() { decorated.failNamespace.Store(false) })
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-namespace-lookup")
	decorated.failNamespace.Store(true)
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Process: apiv2.ProjectProcessTemplate{
			LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "application"},
		}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
		return decorated.namespaceLookups.Load() > 0 && len(current.Status.Conditions) != 0 &&
			strings.Contains(current.Status.Conditions[0].Message, "injected Project namespace lookup failure"), nil
	})
	require.Equal(t, apiv2.ProjectPhasePending, pending.Status.Phase)
	requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparing)
	decorated.failNamespace.Store(false)
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that ProjectContainer distinguishes preparation failure from loss of an established context,
// retaining successful preparation through Unknown and subsequent Pending physical resource observations.
func TestV2ProjectContainerContextLossReasons(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"preparing", "prepared", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectPhysicalClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectPhysicalClient{Client: apiClient}
					decorated.reportContainerUnknown.Store(scenario == "preparing")
					return decorated
				}})
			require.NoError(t, startupErr)
			t.Cleanup(func() {
				decorated.reportContainerUnknown.Store(false)
				decorated.reportContainerPending.Store(false)
			})
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-context-loss-"+scenario)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
				}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			expectedReason := apiv2.ProjectReasonMissing
			var observed *apiv2.ProjectContainer
			if scenario == "preparing" {
				expectedReason = apiv2.ProjectReasonPreparationFailed
				observed = waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return current.Status.ContainerID != "" && len(current.Status.Conditions) != 0 &&
						strings.Contains(current.Status.Conditions[0].Message, "PhysicalContainer reports"), nil
				})
				require.Equal(t, apiv2.ProjectPhasePending, observed.Status.Phase)
				requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparing)
			} else {
				// A started container makes later Pending observations a regression rather than a container awaiting its first session.
				observed = startProjectContainerAdapter(t, ctx, server.Client, server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator),
					waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady))
				if scenario == "unknown" {
					decorated.reportContainerUnknown.Store(true)
					require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
						[]byte(`{"metadata":{"annotations":{"context-observation":"unknown"}}}`))))
					unknown := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseUnknown)
					requireReadyCondition(t, unknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonLookupFailed)
					decorated.reportContainerPending.Store(true)
					decorated.reportContainerUnknown.Store(false)
					require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
						[]byte(`{"metadata":{"annotations":{"context-observation":"pending"}}}`))))
					pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
						return current.Status.Phase == apiv2.ProjectPhaseUnknown && len(current.Status.Conditions) != 0 &&
							strings.Contains(current.Status.Conditions[0].Message, "injected pending physical resource observation"), nil
					})
					requireReadyCondition(t, pending.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonLookupFailed)
				}
			}
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			removeProjectRuntimeContainer(t, ctx, orchestrator, observed.Status.ContainerID)
			waitObjectAssumesStateEx(t, ctx, server.Client, types.NamespacedName{Namespace: namespace.Name, Name: observed.Status.PhysicalContainerRef},
				func(physical *apiv2.PhysicalContainer) (bool, error) {
					return physical.Status.Phase == apiv2.PhysicalContainerPhaseFailed ||
						physical.Status.Phase == apiv2.PhysicalContainerPhaseExited, nil
				})
			decorated.reportContainerUnknown.Store(false)
			decorated.reportContainerPending.Store(false)
			require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"context-observation":"failed"}}}`))))
			failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, expectedReason)
			require.Contains(t, failed.Status.Conditions[0].Message, "PhysicalContainer reports")
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
		})
	}
}

// Verifies that ProjectContainer recovers from physical resource lookup errors during startup and after preparation,
// eventually becoming ready with the same container and log identity without creating a replacement.
func TestV2ProjectContainerRecoversFromPhysicalLookupErrors(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectPhysicalClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &projectPhysicalClient{Client: client}
			return decorated
		}})
	require.NoError(t, startupErr)
	t.Cleanup(func() { decorated.failContainerLists.Store(false) })
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-observation-recovery")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"not-started"}}},
			Container: apiv2.ProjectContainerTemplate{
				LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
			}}}
	decorated.failContainerLists.Store(true)
	require.NoError(t, server.Client.Create(ctx, resource))
	waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(_ *apiv2.ProjectContainer) (bool, error) {
		return decorated.containerListFailures.Load() > 0, nil
	})
	decorated.failContainerLists.Store(false)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"observation-probe":"start"}}}`))))
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	initialFailures := decorated.containerListFailures.Load()
	decorated.failContainerLists.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"observation-probe":"fail"}}}`))))
	waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(_ *apiv2.ProjectContainer) (bool, error) {
		return decorated.containerListFailures.Load() > initialFailures, nil
	})
	decorated.failContainerLists.Store(false)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"observation-probe":"recover"}}}`))))
	recovered := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	requireReadyCondition(t, recovered.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
	require.Equal(t, prepared.Status.ContainerID, recovered.Status.ContainerID)
	require.Equal(t, prepared.Status.RuntimeLogs, recovered.Status.RuntimeLogs)
	var physicalResources apiv2.PhysicalContainerList
	require.NoError(t, server.Client.List(ctx, &physicalResources, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, physicalResources.Items, 1)
	require.Equal(t, prepared.Status.RuntimeLogs.UID, physicalResources.Items[0].UID)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that Stop selects the ProjectContainer Stopping handler before physical resource lookup and remains
// Stopping while lookup fails, then finishes cleanup after reconciliation can observe the physical resource.
func TestV2ProjectContainerStopPrecedesPhysicalLookup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectPhysicalClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &projectPhysicalClient{Client: client}
			return decorated
		}})
	require.NoError(t, startupErr)
	t.Cleanup(func() { decorated.failContainerLists.Store(false) })
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-stop-lookup")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	decorated.failContainerLists.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	stopping := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseStopping && decorated.containerListFailures.Load() != 0, nil
	})
	requireReadyCondition(t, stopping.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonStopping)
	decorated.failContainerLists.Store(false)
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that ProjectContainer observes container lifecycle state without gating it on the physical
// condition's generation, and that Failed survives later Running observations, Stop and cleanup lookup errors.
func TestV2ProjectContainerObservesContainerWithoutGenerationGate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectPhysicalClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				decorated = &projectPhysicalClient{Client: client}
				decorated.staleContainerCondition.Store(true)
				return decorated
			},
		})
	require.NoError(t, startupErr)
	t.Cleanup(func() {
		decorated.failContainerLists.Store(false)
		decorated.reportContainerRunning.Store(false)
	})
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-observed-generation")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	removeProjectRuntimeContainer(t, ctx, orchestrator, prepared.Status.ContainerID)
	failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
	decorated.reportContainerRunning.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	stillFailed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseFailed && current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
	})
	require.Equal(t, failed.Status.Conditions[0].Message, stillFailed.Status.Conditions[0].Message)
	decorated.failContainerLists.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"project-terminal-lookup":"failed"}}}`))))
	lookupFailed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return decorated.containerListFailures.Load() != 0 && current.Status.Phase == apiv2.ProjectPhaseFailed, nil
	})
	require.Equal(t, failed.Status.Conditions[0].Reason, lookupFailed.Status.Conditions[0].Reason)
	require.Equal(t, failed.Status.Conditions[0].Message, lookupFailed.Status.Conditions[0].Message)
	decorated.reportContainerRunning.Store(false)
	previousLookupFailures := decorated.containerListFailures.Load()
	require.NoError(t, server.Client.Delete(ctx, resource))
	physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}}
	deleting := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return !current.DeletionTimestamp.IsZero() && decorated.containerListFailures.Load() > previousLookupFailures, nil
	})
	require.False(t, deleting.DeletionTimestamp.IsZero())
	require.Equal(t, apiv2.ProjectPhaseFailed, deleting.Status.Phase)
	require.Equal(t, failed.Status.Conditions[0].Reason, deleting.Status.Conditions[0].Reason)
	require.Equal(t, failed.Status.Conditions[0].Message, deleting.Status.Conditions[0].Message)
	decorated.failContainerLists.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
}

func (apiClient *projectPhysicalClient) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, namespace := obj.(*apiv2.Namespace); namespace && apiClient.failNamespace.Load() {
		apiClient.namespaceLookups.Add(1)
		return errors.New("injected Project namespace lookup failure")
	}
	if _, network := obj.(*apiv2.LogicalContainerNetwork); network && key.Name == "later" && apiClient.failNetwork.Load() {
		apiClient.networkLookups.Add(1)
		return errors.New("later network lookup unavailable")
	}
	return apiClient.Client.Get(ctx, key, obj, options...)
}

func (apiClient *projectPhysicalClient) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption) error {
	if owner := metav1.GetControllerOf(obj); owner != nil && owner.Kind == "ProjectContainer" &&
		!obj.GetDeletionTimestamp().IsZero() && !slices.Contains(obj.GetFinalizers(), controllers.LogicalPhysicalHoldFinalizer) &&
		apiClient.loseHoldResponse.CompareAndSwap(true, false) {
		if patchErr := apiClient.Client.Patch(ctx, obj, patch, options...); patchErr != nil {
			return patchErr
		}
		apiClient.holdResponseLost.Store(true)
		return errors.New("Project hold release response lost")
	}
	return apiClient.Client.Patch(ctx, obj, patch, options...)
}

func (apiClient *projectPhysicalClient) Status() ctrl_client.SubResourceWriter {
	return &projectPhysicalStatusWriter{SubResourceWriter: apiClient.Client.Status(), client: apiClient}
}

type projectPhysicalStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *projectPhysicalClient
}

type projectPhysicalReader struct {
	ctrl_client.Reader
	failLists    atomic.Bool
	listFailures atomic.Int32
}

func (reader *projectPhysicalReader) Get(ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, container := object.(*apiv2.PhysicalContainer); container && reader.failLists.Load() {
		reader.listFailures.Add(1)
		return errors.New("uncached Project physical resource confirmation unavailable")
	}
	return reader.Reader.Get(ctx, key, object, options...)
}

func (writer *projectPhysicalStatusWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	complete := false
	switch physical := obj.(type) {
	case *apiv2.PhysicalProcess:
		complete = writer.client.kind == "process" && physical.Status.LogsComplete
	case *apiv2.PhysicalContainerImage:
		complete = writer.client.kind == "image" && physical.Status.LogsComplete
	}
	if complete && !obj.GetDeletionTimestamp().IsZero() && writer.client.blockFinalStatus.Load() {
		writer.client.finalStatusOnce.Do(func() { writer.client.finalStatus <- obj.DeepCopyObject().(ctrl_client.Object) })
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "projectphysicalresources"},
			obj.GetName(), errors.New("Project physical resource final status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

// Verifies that ProjectContainer requests child deletion immediately, retains both finalizers through
// cleanup status conflicts, and retries a committed logical-hold release with a lost response.
func TestV2ProjectContainerDeletionRetainsPhysicalUntilCleanupIsDurable(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"image"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectPhysicalClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectPhysicalClient{Client: apiClient, kind: kind, finalStatus: make(chan ctrl_client.Object, 1)}
					return decorated
				}})
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-durable-"+kind)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			physicalResource := &apiv2.PhysicalContainerImage{}
			cleanupFinalizer := apiv2.GroupName + "/physicalcontainerimage-reconciler"

			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			source := namespace.Name + "-image"
			releasePull := sync.OnceFunc(orchestrator.BlockPullImage(source))
			t.Cleanup(releasePull)
			resource.Spec.Container = apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source},
				Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing"}}},
			}

			require.NoError(t, server.Client.Create(ctx, resource))

			waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				return current.Status.StartupLogs != nil && current.Status.StartupLogs.Available && !current.Status.StartupLogs.Complete, nil
			})

			name := types.NamespacedName{Namespace: namespace.Name, Name: "project-" + string(resource.UID)}
			require.NoError(t, server.Client.Get(ctx, name, physicalResource))
			require.Contains(t, physicalResource.GetFinalizers(), controllers.LogicalPhysicalHoldFinalizer)
			require.Contains(t, physicalResource.GetFinalizers(), cleanupFinalizer)
			decorated.blockFinalStatus.Store(true)
			t.Cleanup(func() { decorated.blockFinalStatus.Store(false) })
			decorated.loseHoldResponse.Store(true)
			require.NoError(t, server.Client.Delete(ctx, resource))
			select {
			case <-decorated.finalStatus:
			case <-ctx.Done():
				t.Fatal("Project physical resource cleanup did not publish its disposition")
			}
			require.NoError(t, server.Client.Get(ctx, name, physicalResource))
			require.False(t, physicalResource.GetDeletionTimestamp().IsZero())
			require.Contains(t, physicalResource.GetFinalizers(), cleanupFinalizer)
			require.Contains(t, physicalResource.GetFinalizers(), controllers.LogicalPhysicalHoldFinalizer)
			require.NoError(t, server.Client.Get(ctx, resource.NamespacedName(), &apiv2.ProjectContainer{}))
			decorated.blockFinalStatus.Store(false)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			waitProjectPhysicalDeleted(t, ctx, server.Client, physicalResource)
			require.True(t, decorated.holdResponseLost.Load())
		})
	}
}

// Verifies that ProjectContainer observes undecoded creates through the cache and deletes
// every owned physical resource, including duplicates with unexpected names and no recorded UID.
func TestV2ProjectContainerDiscoversAndDeletesPhysicalResourcesByOwner(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectPhysicalClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectPhysicalClient{Client: apiClient}
					decorated.hidePhysicalResources.Store(true)
					return decorated
				}})
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-owner-"+kind)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}

			resource.Spec.Container = apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}

			require.NoError(t, server.Client.Create(ctx, resource))
			waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				return decorated.undecodedCreates.Load() >= 1, nil
			})
			decorated.hidePhysicalResources.Store(false)
			require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
				[]byte(`{"metadata":{"annotations":{"cache-observation":"released"}}}`))))
			running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			require.InDelta(t, 1, decorated.undecodedCreates.Load(), 1)
			original := &apiv2.PhysicalContainer{}

			require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.RuntimeLogs.Name}, original))
			duplicate := original.DeepCopyObject().(ctrl_client.Object)
			duplicate.SetName("unexpected-owned-physical")
			duplicate.SetUID("")
			duplicate.SetResourceVersion("")
			duplicate.SetManagedFields(nil)
			require.NoError(t, server.Client.Create(ctx, duplicate))

			// A second owned resource invalidates normal observation, but must not block cleanup.
			unknown := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, unknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
			require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			stillUnknown := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				return current.Status.Phase == apiv2.ProjectPhaseFailed && current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
			})
			requireReadyCondition(t, stillUnknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
			require.Equal(t, running.Status.RuntimeLogs.UID, stillUnknown.Status.RuntimeLogs.UID)
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			waitProjectPhysicalDeleted(t, ctx, server.Client, original)
			waitProjectPhysicalDeleted(t, ctx, server.Client, duplicate)
		})
	}
}

func waitProjectPhysicalDeleted(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, physicalResource ctrl_client.Object) {
	t.Helper()
	switch physical := physicalResource.(type) {
	case *apiv2.PhysicalProcess:
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
	case *apiv2.PhysicalContainer:
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
	case *apiv2.PhysicalContainerImage:
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
	default:
		t.Fatalf("unsupported Project physical resource %T", physicalResource)
	}
}

// Verifies that deletion confirms a recorded undecoded create with a targeted read and retains
// ProjectContainer until that confirmation succeeds, without changing its established preparation lifecycle.
func TestV2ProjectContainerDeletionConfirmsUnobservedCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var reader *projectPhysicalReader
	var decorated *projectPhysicalClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				decorated = &projectPhysicalClient{Client: apiClient}
				decorated.hidePhysicalResources.Store(true)
				return decorated
			},
			DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
				reader = &projectPhysicalReader{Reader: apiReader}
				return reader
			},
		})
	require.NoError(t, startupErr)
	decorated.onUndecodedCreate = func(object ctrl_client.Object) {
		if _, isContainer := object.(*apiv2.PhysicalContainer); isContainer {
			reader.failLists.Store(true)
		}
	}
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-delete-confirmation")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"not-started"}}},
			Container: apiv2.ProjectContainerTemplate{
				LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: namespace.Name + "-image"},
			}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return decorated.undecodedCreates.Load() == 1 && current.Status.Phase == apiv2.ProjectPhasePending, nil
	})
	t.Cleanup(func() { reader.failLists.Store(false) })
	require.NoError(t, server.Client.Delete(ctx, resource))
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return !current.DeletionTimestamp.IsZero() && reader.listFailures.Load() > 0, nil
	})
	require.Equal(t, apiv2.ProjectPhasePending, failed.Status.Phase)
	require.Contains(t, failed.Finalizers, apiv2.GroupName+"/projectcontainer-reconciler")
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: "project-" + string(resource.UID)}, physical))
	require.True(t, physical.DeletionTimestamp.IsZero())
	reader.failLists.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
}

// Verifies that ProjectContainer evaluates later network failures and lookup errors despite an earlier
// missing reference, and delegates readiness after a logical network publishes its physical resource reference.
func TestV2ProjectContainerChecksAllNetworkReferences(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"failed", "lookup", "published"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectPhysicalClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectPhysicalClient{Client: apiClient}
					return decorated
				}})
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-network-scan-"+scenario)
			later := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "later", Namespace: namespace.Name},
				Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}}}
			require.NoError(t, server.Client.Create(ctx, later))
			later.Status.Phase = apiv2.LogicalContainerNetworkPhaseFailed
			later.Status.Conditions = []metav1.Condition{{Type: string(apiv2.ConditionReady), Status: metav1.ConditionFalse,
				ObservedGeneration: later.Generation, LastTransitionTime: metav1.Now(), Reason: "NetworkDenied", Message: "later failed"}}
			networks := []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing"}, {NetworkRef: later.Name}}
			if scenario == "published" {
				later.Status.Phase = apiv2.LogicalContainerNetworkPhasePending
				later.Status.PhysicalNetworkRef = "published-physical"
				later.Status.Conditions = nil
				networks = networks[1:]
			}
			require.NoError(t, server.Client.Status().Update(ctx, later))
			decorated.failNetwork.Store(scenario == "lookup")
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "external-image", Networks: networks}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			switch scenario {
			case "failed":
				failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
				requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
				require.Contains(t, failed.Status.Conditions[0].Message, "later failed")
			case "lookup":
				pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return len(current.Status.Conditions) != 0 &&
						current.Status.Conditions[0].Reason == string(apiv2.ProjectReasonPreparing) &&
						strings.Contains(current.Status.Conditions[0].Message, "later network lookup unavailable"), nil
				})
				require.Equal(t, apiv2.ProjectPhasePending, pending.Status.Phase)
				require.Positive(t, decorated.networkLookups.Load())
				decorated.failNetwork.Store(false)
				waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
			case "published":
				pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return current.Status.PhysicalContainerRef != "", nil
				})
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: pending.Status.PhysicalContainerRef}, physical))
				require.Equal(t, "published-physical", physical.Spec.Container.Networks[0].NetworkRef)
			}
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
		})
	}
}

type projectCreateCollisionClient struct {
	ctrl_client.Client
	kind     string
	scenario string
	injected atomic.Bool
}

func (apiClient *projectCreateCollisionClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	owner := metav1.GetControllerOf(obj)
	_, image := obj.(*apiv2.PhysicalContainerImage)
	_, container := obj.(*apiv2.PhysicalContainer)
	if owner == nil || owner.Kind != "ProjectContainer" || (apiClient.kind == "image" && !image) ||
		(apiClient.kind == "container" && !container) || !apiClient.injected.CompareAndSwap(false, true) {
		return apiClient.Client.Create(ctx, obj, options...)
	}
	existing := obj.DeepCopyObject().(ctrl_client.Object)
	switch apiClient.scenario {
	case "owner":
		existing.SetOwnerReferences(nil)
	case "hold":
		existing.SetFinalizers([]string{"tests.dcp.microsoft.com/collision"})
	case "spec":
		switch physical := existing.(type) {
		case *apiv2.PhysicalContainer:
			physical.Spec.Container.Command = []string{"different"}
		case *apiv2.PhysicalContainerImage:
			physical.Spec.Image.Image += "-different"
		}
	}
	if createErr := apiClient.Client.Create(ctx, existing, options...); createErr != nil {
		return createErr
	}
	resource := "physicalcontainers"
	if image {
		resource = "physicalcontainerimages"
	}
	return apierrors.NewAlreadyExists(schema.GroupResource{Group: apiv2.GroupName, Resource: resource}, obj.GetName())
}

// Verifies that ProjectContainer validates exact-name AlreadyExists collisions for physical containers
// and images, accepting owned container declarations and rejecting incompatible identities or image inputs.
func TestV2ProjectContainerValidatesPhysicalCreateCollisions(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"image", "container"} {
		for _, scenario := range []string{"compatible", "owner", "hold", "spec"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
				t.Cleanup(cancel)
				server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
					TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
						return &projectCreateCollisionClient{Client: client, kind: kind, scenario: scenario}
					}})
				require.NoError(t, startupErr)
				namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-collision-"+kind+"-"+scenario)
				resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
					Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
						LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
					}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
				require.NoError(t, server.Client.Create(ctx, resource))
				expected := apiv2.ProjectPhaseFailed
				if scenario == "compatible" || (kind == "container" && scenario == "spec") {
					expected = apiv2.ProjectPhaseReady
				}
				observed := waitProjectPhase(t, ctx, server.Client, resource, expected)
				if expected == apiv2.ProjectPhaseFailed {
					requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
				}
				if scenario == "hold" || scenario == "owner" {
					var collision ctrl_client.Object = &apiv2.PhysicalContainer{}
					if kind == "image" {
						collision = &apiv2.PhysicalContainerImage{}
					}
					require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: "project-" + string(resource.UID)}, collision))
					if scenario == "hold" {
						require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
							if getErr := server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(collision), collision); getErr != nil {
								return getErr
							}
							collision.SetFinalizers(nil)
							return server.Client.Update(ctx, collision)
						}))
					} else {
						removeProjectPhysicalHold(t, ctx, server.Client, collision)
					}
					require.NoError(t, server.Client.Delete(ctx, collision))
				}
				require.NoError(t, server.Client.Delete(ctx, resource))
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			})
		}
	}
}

type projectLostCreateClient struct {
	ctrl_client.Client
	responseLost atomic.Bool
	created      chan ctrl_client.Object
	release      chan struct{}
}

func (apiClient *projectLostCreateClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := apiClient.Client.Create(ctx, obj, options...)
	owner := metav1.GetControllerOf(obj)
	if _, container := obj.(*apiv2.PhysicalContainer); container && createErr == nil &&
		owner != nil && owner.Kind == "ProjectContainer" && apiClient.responseLost.CompareAndSwap(false, true) {
		apiClient.created <- obj.DeepCopyObject().(ctrl_client.Object)
		select {
		case <-apiClient.release:
		case <-ctx.Done():
		}
		return errors.New("Project container create response was lost")
	}
	return createErr
}

// Verifies that stop and deletion settle an ambiguous prepared-container create without
// creating another container or invoking the application command.
func TestV2ProjectContainerRecoversLostCreateAndMutableStop(t *testing.T) {
	t.Parallel()
	for _, deletion := range []bool{false, true} {
		name := "stop"
		if deletion {
			name = "delete"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectLostCreateClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectLostCreateClient{Client: apiClient, created: make(chan ctrl_client.Object, 1), release: make(chan struct{})}
					return decorated
				}})
			require.NoError(t, startupErr)
			release := sync.OnceFunc(func() { close(decorated.release) })
			t.Cleanup(release)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-lost-context-"+name)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Command: []string{"application"}},
				}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			var physicalResource ctrl_client.Object
			select {
			case physicalResource = <-decorated.created:
			case <-ctx.Done():
				t.Fatal("container was not declared")
			}
			if deletion {
				require.NoError(t, server.Client.Delete(ctx, resource))
			} else {
				require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			}
			release()
			if !deletion {
				stopped := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
				require.Equal(t, []string{"application"}, stopped.Status.EffectiveArgs)
				require.NoError(t, server.Client.Delete(ctx, resource))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: physicalResource.GetName(), Namespace: namespace.Name}}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: physicalResource.GetName(), Namespace: namespace.Name}}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, image)
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.LessOrEqual(t, orchestrator.CreateContainerCallCount(physicalResource.GetName()), 1)
		})
	}
}

type projectProcessLostCreateClient struct {
	ctrl_client.Client
	created  chan *apiv2.PhysicalProcess
	release  chan struct{}
	attempts atomic.Int32
}

func (apiClient *projectProcessLostCreateClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	physical, isProcess := obj.(*apiv2.PhysicalProcess)
	owner := metav1.GetControllerOf(obj)
	if !isProcess || owner == nil || owner.Kind != "ProjectProcess" {
		return apiClient.Client.Create(ctx, obj, options...)
	}
	if apiClient.attempts.Add(1) != 1 {
		return apiClient.Client.Create(ctx, obj, options...)
	}
	committed := physical.DeepCopy()
	if createErr := apiClient.Client.Create(ctx, committed, options...); createErr != nil {
		return createErr
	}
	apiClient.created <- committed
	select {
	case <-apiClient.release:
		return errors.New("Project process create committed without a decoded response")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (apiClient *projectProcessLostCreateClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if listErr := apiClient.Client.List(ctx, list, options...); listErr != nil {
		return listErr
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if processes, isProcessList := list.(*apiv2.PhysicalProcessList); isProcessList && listOptions.FieldSelector != nil &&
		strings.Contains(listOptions.FieldSelector.String(), ".metadata.projectProcessOwnerUID") {
		processes.Items = nil
	}
	return nil
}

type projectFailedServiceReader struct {
	ctrl_client.Reader
	fail atomic.Bool
}

func (reader *projectFailedServiceReader) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if getErr := reader.Reader.Get(ctx, key, obj, options...); getErr != nil {
		return getErr
	}
	if service, isService := obj.(*apiv2.LogicalService); isService && reader.fail.Load() {
		service.Status.Phase = apiv2.LogicalServicePhaseFailed
	}
	return nil
}

// Verifies that an undecoded direct-process create is adopted without reallocating its inputs
// or relaunching, and that stop or deletion settles the in-flight create and physical cleanup.
func TestV2ProjectProcessRecoversLostCreateAndShutdown(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"recover", "stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectProcessLostCreateClient
			var serviceReader *projectFailedServiceReader
			server, environment, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						decorated = &projectProcessLostCreateClient{
							Client: apiClient, created: make(chan *apiv2.PhysicalProcess, 1), release: make(chan struct{}),
						}
						return decorated
					},
					DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
						serviceReader = &projectFailedServiceReader{Reader: apiReader}
						return serviceReader
					},
				})
			require.NoError(t, startupErr)
			release := sync.OnceFunc(func() { close(decorated.release) })
			t.Cleanup(release)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-process-lost-"+action)
			resource := serviceProjectProcess("project", namespace.Name, t.TempDir(),
				[]string{`--port={{portForServing "http"}}`}, []commonapi.EnvVar{{Name: "PORT", Value: `{{portForServing "http"}}`}})
			resource.Spec.LaunchMode = apiv2.ProjectLaunchModeDirect
			require.NoError(t, server.Client.Create(ctx, resource))
			var declared *apiv2.PhysicalProcess
			select {
			case declared = <-decorated.created:
			case <-ctx.Done():
				t.Fatal("Project did not declare its physical process")
			}
			physical := waitObjectAssumesStateEx(t, ctx, server.Client, declared.NamespacedName(), func(current *apiv2.PhysicalProcess) (bool, error) {
				return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, nil
			})
			require.Equal(t, declared.UID, physical.UID)
			require.Len(t, environment.TestProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil), 1)
			serviceReader.fail.Store(true)
			switch action {
			case "stop":
				require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			case "delete":
				require.NoError(t, server.Client.Delete(ctx, resource))
			}
			release()
			if action == "recover" {
				running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseActive)
				requireReadyCondition(t, running.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonApplicationRunning)
				require.Equal(t, physical.Name, running.Status.PhysicalProcessRef)
				require.Len(t, running.Status.Ports, 1)
				require.Equal(t, []string{fmt.Sprintf("--port=%d", running.Status.Ports[0].Port)}, physical.Spec.Process.Args)
				require.Equal(t, []commonapi.EnvVar{{Name: "PORT", Value: fmt.Sprint(running.Status.Ports[0].Port)}}, physical.Spec.Process.Env)
				require.Equal(t, physical.Spec.Process.Args, running.Status.EffectiveArgs)
				require.Equal(t, physical.Spec.Process.Env, running.Status.EffectiveEnv)
			} else if action == "stop" {
				waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
			}
			if action != "delete" {
				require.NoError(t, server.Client.Delete(ctx, resource))
			}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			require.EqualValues(t, 1, decorated.attempts.Load())
			require.Len(t, environment.TestProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil), 1)
		})
	}
}

// Verifies that ProjectProcess does not adopt an owned physical process whose launch command or
// lifetime policy does not match the prepared application configuration.
func TestV2ProjectProcessRejectsIncompatibleOwnedPhysical(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"command", "retained"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createActiveV2Namespace(t, ctx, "v2-project-process-collision-"+scenario)
			start := false
			resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{
					RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect, Start: &start,
				}, Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
					ExecutablePath: namespace.Name + "-command", Args: []string{"expected"},
					AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
				}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
			require.NoError(t, client.Create(ctx, resource))
			prepared := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			requireReadyCondition(t, prepared.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonAwaitingStart)

			physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{
				Name: "project-" + string(resource.UID), Namespace: namespace.Name,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(resource, apiv2.GroupVersion.WithKind("ProjectProcess"))},
			}, Spec: apiv2.PhysicalProcessSpec{Stop: true, Process: &apiv2.PhysicalProcessConfig{
				ExecutablePath: resource.Spec.Process.ExecutablePath, Args: []string{"expected"},
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
			}}}
			switch scenario {
			case "command":
				physical.Spec.Process.ExecutablePath += "-different"
			case "retained":
				physical.Spec.Process.RetainRuntimeProcess = true
			}
			controllers.InitializePhysicalResourceFinalizers(physical, apiv2.GroupName+"/physicalprocess-reconciler")
			require.NoError(t, client.Create(ctx, physical))
			cleanupProjectPhysical(t, physical)
			require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"start":true}}`))))
			waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
				return current.Status.Phase == apiv2.ProjectPhasePending && ready != nil && strings.Contains(ready.Message, "different spec"), nil
			})
			require.Empty(t, testProcessExecutor.FindAll([]string{resource.Spec.Process.ExecutablePath}, "", nil))
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, physical)
		})
	}
}

type projectFailedStopObserver struct {
	ctrl_client.Client
	processStop   atomic.Bool
	containerStop atomic.Bool
	processSeen   chan struct{}
	containerSeen chan struct{}
	processOnce   sync.Once
	containerOnce sync.Once
}

func (observer *projectFailedStopObserver) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if getErr := observer.Client.Get(ctx, key, obj, options...); getErr != nil {
		return getErr
	}
	switch project := obj.(type) {
	case *apiv2.ProjectProcess:
		if project.Spec.Stop {
			observer.processStop.Store(true)
		}
	case *apiv2.ProjectContainer:
		if project.Spec.Stop {
			observer.containerStop.Store(true)
		}
	}
	return nil
}

func (observer *projectFailedStopObserver) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if listErr := observer.Client.List(ctx, list, options...); listErr != nil {
		return listErr
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if listOptions.FieldSelector == nil {
		return nil
	}
	selector := listOptions.FieldSelector.String()
	if _, isProcess := list.(*apiv2.PhysicalProcessList); isProcess && observer.processStop.Load() &&
		strings.Contains(selector, ".metadata.projectProcessOwnerUID") {
		observer.processOnce.Do(func() { close(observer.processSeen) })
	}
	if _, isContainer := list.(*apiv2.PhysicalContainerList); isContainer && observer.containerStop.Load() &&
		strings.Contains(selector, ".metadata.projectContainerOwnerUID") {
		observer.containerOnce.Do(func() { close(observer.containerSeen) })
	}
	return nil
}

// Verifies that a later stop request does not replace a direct Project's terminal physical-failure
// diagnosis, while deletion still cleans up the owned execution.
func TestV2ProjectDirectFailureRemainsTerminalAfterStop(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var observer *projectFailedStopObserver
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			observer = &projectFailedStopObserver{Client: apiClient, processSeen: make(chan struct{}), containerSeen: make(chan struct{})}
			return observer
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-direct-failed-stop")
	processProject := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "process", Namespace: namespace.Name},
		Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect},
			Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
	containerProject := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect},
			Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Entrypoint: "application",
			}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, server.Client.Create(ctx, processProject))
	require.NoError(t, server.Client.Create(ctx, containerProject))
	processRunning := waitProjectPhase(t, ctx, server.Client, processProject, apiv2.ProjectPhaseActive)
	containerRunning := waitProjectPhase(t, ctx, server.Client, containerProject, apiv2.ProjectPhaseActive)
	physicalProcess := &apiv2.PhysicalProcess{}
	physicalContainer := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: processRunning.Status.PhysicalProcessRef}, physicalProcess))
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: containerRunning.Status.PhysicalContainerRef}, physicalContainer))
	removeProjectPhysicalHold(t, ctx, server.Client, physicalProcess)
	removeProjectPhysicalHold(t, ctx, server.Client, physicalContainer)
	processFailed := waitProjectPhase(t, ctx, server.Client, processProject, apiv2.ProjectPhaseFailed)
	containerFailed := waitProjectPhase(t, ctx, server.Client, containerProject, apiv2.ProjectPhaseFailed)
	requireReadyCondition(t, processFailed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
	requireReadyCondition(t, containerFailed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonMissing)
	require.NoError(t, server.Client.Patch(ctx, processProject, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	require.NoError(t, server.Client.Patch(ctx, containerProject, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	for _, observed := range []<-chan struct{}{observer.processSeen, observer.containerSeen} {
		select {
		case <-observed:
		case <-ctx.Done():
			t.Fatal("Project did not reconcile the stop request")
		}
	}
	currentProcess := &apiv2.ProjectProcess{}
	currentContainer := &apiv2.ProjectContainer{}
	require.NoError(t, server.Client.Get(ctx, processProject.NamespacedName(), currentProcess))
	require.NoError(t, server.Client.Get(ctx, containerProject.NamespacedName(), currentContainer))
	require.Equal(t, apiv2.ProjectPhaseFailed, currentProcess.Status.Phase)
	require.Equal(t, processFailed.Status.Conditions[0].Reason, currentProcess.Status.Conditions[0].Reason)
	require.Equal(t, processFailed.Status.Conditions[0].Message, currentProcess.Status.Conditions[0].Message)
	require.Equal(t, apiv2.ProjectPhaseFailed, currentContainer.Status.Phase)
	require.Equal(t, containerFailed.Status.Conditions[0].Reason, currentContainer.Status.Conditions[0].Reason)
	require.Equal(t, containerFailed.Status.Conditions[0].Message, currentContainer.Status.Conditions[0].Message)
	require.NoError(t, server.Client.Delete(ctx, processProject))
	require.NoError(t, server.Client.Delete(ctx, containerProject))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, processProject)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, containerProject)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physicalProcess)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physicalContainer)
}

type projectStatusConflictClient struct {
	ctrl_client.Client
	conflicted atomic.Bool
}

func (apiClient *projectStatusConflictClient) Status() ctrl_client.SubResourceWriter {
	return &projectStatusConflictWriter{SubResourceWriter: apiClient.Client.Status(), client: apiClient}
}

type projectStatusConflictWriter struct {
	ctrl_client.SubResourceWriter
	client *projectStatusConflictClient
}

func (writer *projectStatusConflictWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	if resource, project := obj.(*apiv2.ProjectContainer); project &&
		resource.Status.Phase == apiv2.ProjectPhaseFailed &&
		resource.Status.RuntimeLogs != nil && resource.Status.RuntimeLogs.Complete && !resource.Status.RuntimeLogs.Available &&
		writer.client.conflicted.CompareAndSwap(false, true) {
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "projectcontainers"}, resource.Name, errors.New("injected ProjectContainer status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

// Verifies that forced disappearance of a prepared container retains either its observed termination
// failure or missing-context outcome, preserves capture identity through conflicts, and completes followers.
func TestV2ProjectContainerMissingPhysicalCompletesFollowerThroughConflict(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectStatusConflictClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			decorated = &projectStatusConflictClient{Client: apiClient}
			return decorated
		}})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-missing-context")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	prepared := startProjectContainerAdapter(t, ctx, server.Client, orchestrator, waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady))
	require.NoError(t, orchestrator.SimulateContainerLogging(prepared.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("before-delete\n")))
	follow, followErr := server.RestClient.Get().
		AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projectcontainers", resource.Name, "log").
		Param("follow", "true").Stream(ctx)
	require.NoError(t, followErr)
	defer follow.Close()
	initial := make([]byte, len("before-delete\n"))
	_, readErr := io.ReadFull(follow, initial)
	require.NoError(t, readErr)
	physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}}
	require.NoError(t, server.Client.Delete(ctx, physical))
	removeProjectPhysicalHold(t, ctx, server.Client, physical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		if ready == nil {
			return false, nil
		}
		terminal := current.Status.Phase == apiv2.ProjectPhaseFailed &&
			ready.Reason == string(apiv2.ProjectReasonMissing)
		return terminal && current.Status.RuntimeLogs != nil &&
			current.Status.RuntimeLogs.Complete && !current.Status.RuntimeLogs.Available, nil
	})
	require.True(t, decorated.conflicted.Load())
	require.Equal(t, prepared.Status.RuntimeLogs.UID, failed.Status.RuntimeLogs.UID)
	require.True(t, failed.Status.RuntimeLogs.Complete)
	remaining, drainErr := io.ReadAll(follow)
	require.NoError(t, drainErr)
	require.Empty(t, remaining)
}

func cleanupProjectPhysical(t *testing.T, physicalResource ctrl_client.Object) {
	t.Helper()
	key := ctrl_client.ObjectKeyFromObject(physicalResource)
	uid := physicalResource.GetUID()
	t.Cleanup(func() {
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		defer cancel()
		current := physicalResource.DeepCopyObject().(ctrl_client.Object)
		require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if getErr := client.Get(ctx, key, current); getErr != nil {
				return ctrl_client.IgnoreNotFound(getErr)
			}
			if current.GetUID() != uid {
				return fmt.Errorf("project physical resource %s was replaced before cleanup", key)
			}
			if deleteErr := client.Delete(ctx, current); deleteErr != nil {
				return ctrl_client.IgnoreNotFound(deleteErr)
			}
			return ctrl_client.IgnoreNotFound(controllers.ReleaseLogicalPhysicalHoldFinalizer(ctx, client, current))
		}))
	})
}

func removeProjectPhysicalHold(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, physicalResource ctrl_client.Object) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if getErr := apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(physicalResource), physicalResource); getErr != nil {
			return getErr
		}
		return controllers.ReleaseLogicalPhysicalHoldFinalizer(ctx, apiClient, physicalResource)
	}))
}
