/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type projectBackingClient struct {
	ctrl_client.Client
	kind                    string
	blockFinalStatus        atomic.Bool
	finalStatus             chan ctrl_client.Object
	finalStatusOnce         sync.Once
	loseHoldResponse        atomic.Bool
	holdResponseLost        atomic.Bool
	hideBackings            atomic.Bool
	undecodedCreates        atomic.Int32
	failNetwork             atomic.Bool
	networkLookups          atomic.Int32
	failContainerLists      atomic.Bool
	containerListFailures   atomic.Int32
	failEndpointLists       atomic.Bool
	endpointListFailures    atomic.Int32
	staleContainerCondition atomic.Bool
	reportContainerRunning  atomic.Bool
	confirmationReader      *projectBackingReader
}

// Verifies that an Unknown Project preserves its original readable capture on ambiguous discovery
// or logical-hold loss, including existing followers and clients opening logs after the transition.
// Later capture completion is observed without reopening the Unknown lifecycle.
func TestV2ProjectUnknownPreservesRuntimeLogs(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"ambiguous", "hold-removed"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startupErr := StartTestEnvironment(t, ctx, projectTestControllers, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-logs-"+scenario)
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
				}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.NoError(t, orchestrator.SimulateContainerLogging(prepared.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("before-unknown\n")))
			follow, followErr := server.RestClient.Get().
				AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projects", resource.Name, "log").
				Param("follow", "true").Stream(ctx)
			require.NoError(t, followErr)
			defer follow.Close()
			first := make([]byte, len("before-unknown\n"))
			_, firstErr := io.ReadFull(follow, first)
			require.NoError(t, firstErr)
			require.Equal(t, "before-unknown\n", string(first))
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
			var duplicate *apiv2.PhysicalContainer
			if scenario == "ambiguous" {
				duplicate = physical.DeepCopy()
				duplicate.ObjectMeta = metav1.ObjectMeta{Name: "duplicate", Namespace: namespace.Name,
					OwnerReferences: physical.OwnerReferences, Finalizers: physical.Finalizers}
				require.NoError(t, server.Client.Create(ctx, duplicate))
			} else {
				removeProjectBackingHold(t, ctx, server.Client, physical)
			}
			unknown := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseUnknown)
			requireReadyCondition(t, unknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPhysicalExecutionChanged)
			require.NoError(t, server.Client.Get(ctx, physical.NamespacedName(), physical))
			require.Equal(t, physical.GetLogSource(), unknown.Status.RuntimeLogs)
			require.True(t, unknown.Status.RuntimeLogs.Available)
			require.False(t, unknown.Status.RuntimeLogs.Complete)
			require.NoError(t, orchestrator.SimulateContainerLogging(prepared.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("after-unknown\n")))
			next := make([]byte, len("after-unknown\n"))
			_, nextErr := io.ReadFull(follow, next)
			require.NoError(t, nextErr)
			require.Equal(t, "after-unknown\n", string(next))
			late, lateErr := server.RestClient.Get().
				AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projects", resource.Name, "log").
				Stream(ctx)
			require.NoError(t, lateErr)
			defer late.Close()
			output, readErr := io.ReadAll(late)
			require.NoError(t, readErr)
			require.Equal(t, "before-unknown\nafter-unknown\n", string(output))
			if duplicate != nil {
				require.NoError(t, server.Client.Delete(ctx, duplicate))
				removeProjectBackingHold(t, ctx, server.Client, duplicate)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, duplicate)
				require.NoError(t, orchestrator.SimulateContainerStatus(ctx, prepared.Status.ContainerID, containers.ContainerStatusDead))
				completed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseUnknown && current.Status.RuntimeLogs != nil &&
						current.Status.RuntimeLogs.Available && current.Status.RuntimeLogs.Complete, nil
				})
				require.Equal(t, unknown.Status.Conditions[0].Reason, completed.Status.Conditions[0].Reason)
				require.Equal(t, unknown.Status.Conditions[0].Message, completed.Status.Conditions[0].Message)
				require.Equal(t, prepared.Status.RuntimeLogs.UID, completed.Status.RuntimeLogs.UID)
				remaining, drainErr := io.ReadAll(follow)
				require.NoError(t, drainErr)
				require.Empty(t, remaining)
			}
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
		})
	}
}

func (apiClient *projectBackingClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "Project" || !apiClient.hideBackings.Load() {
		return apiClient.Client.Create(ctx, obj, options...)
	}
	committed := obj.DeepCopyObject().(ctrl_client.Object)
	if createErr := apiClient.Client.Create(ctx, committed, options...); createErr != nil {
		return createErr
	}
	apiClient.undecodedCreates.Add(1)
	if _, container := obj.(*apiv2.PhysicalContainer); container && apiClient.confirmationReader != nil {
		apiClient.confirmationReader.failLists.Store(true)
		apiClient.confirmationReader.createErrorPending.Store(true)
	}
	return errors.New("Project backing create committed without a decoded response")
}

func (apiClient *projectBackingClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, endpoints := list.(*apiv2.ServiceEndpointList); endpoints && apiClient.failEndpointLists.Load() {
		apiClient.endpointListFailures.Add(1)
		return errors.New("injected Project endpoint lookup failure")
	}
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
			if apiClient.staleContainerCondition.Load() {
				containers.Items[index].Generation++
			}
			for conditionIndex := range containers.Items[index].Status.Conditions {
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
	if !apiClient.hideBackings.Load() || listOptions.FieldSelector == nil ||
		!strings.Contains(listOptions.FieldSelector.String(), ".metadata.projectOwnerUID") {
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

// Verifies that Project retains its terminal Failed state and original diagnostic after backing
// disappearance or identity replacement, while still completing missing log sources.
func TestV2ProjectFailureSurvivesBackingChanges(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"missing", "replaced"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			server, _, startupErr := StartTestEnvironment(t, ctx, projectTestControllers, t.Name(), t.TempDir())
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-failed-"+scenario)
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
				}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.NoError(t, orchestrator.SimulateContainerStatus(ctx, prepared.Status.ContainerID, containers.ContainerStatusDead))
			failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
			replacement := physical.DeepCopy()
			replacement.ObjectMeta = metav1.ObjectMeta{Name: physical.Name, Namespace: namespace.Name,
				OwnerReferences: physical.OwnerReferences, Finalizers: physical.Finalizers}
			require.NoError(t, server.Client.Delete(ctx, physical))
			removeProjectBackingHold(t, ctx, server.Client, physical)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			if scenario == "replaced" {
				require.NoError(t, server.Client.Create(ctx, replacement))
				require.NotEqual(t, physical.UID, replacement.UID)
			}
			require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			observed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
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
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			var backings apiv2.PhysicalContainerList
			require.NoError(t, server.Client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, backings.Items)
		})
	}
}

// Verifies that Stop selects the Project Stopping handler before backing lookup and remains
// Stopping while lookup fails, then finishes cleanup after reconciliation can observe the backing.
func TestV2ProjectStopPrecedesBackingLookup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectBackingClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			decorated = &projectBackingClient{Client: client}
			return decorated
		}})
	require.NoError(t, startupErr)
	t.Cleanup(func() { decorated.failContainerLists.Store(false) })
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-stop-lookup")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	decorated.failContainerLists.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	stopping := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseStopping && len(current.Status.Conditions) != 0 &&
			current.Status.Conditions[0].Reason == string(apiv2.ProjectReasonPhysicalDeletionFailed), nil
	})
	requireReadyCondition(t, stopping.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPhysicalDeletionFailed)
	decorated.failContainerLists.Store(false)
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseStopped)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that Project observes container lifecycle state without gating it on the physical
// condition's generation, and that Failed survives later Running observations, Stop and retried endpoint errors.
func TestV2ProjectObservesContainerWithoutGenerationGate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectBackingClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				decorated = &projectBackingClient{Client: client}
				decorated.staleContainerCondition.Store(true)
				return decorated
			},
			DecorateAPIReader: func(reader ctrl_client.Reader) ctrl_client.Reader {
				return &projectBackingReader{Reader: reader, endpointFailure: &decorated.failEndpointLists,
					endpointFailures: &decorated.endpointListFailures}
			},
		})
	require.NoError(t, startupErr)
	t.Cleanup(func() {
		decorated.failEndpointLists.Store(false)
		decorated.failContainerLists.Store(false)
		decorated.reportContainerRunning.Store(false)
	})
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-observed-generation")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	decorated.failEndpointLists.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"project-endpoint-probe":"ready"}}}`))))
	stillReady := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return decorated.endpointListFailures.Load() != 0 && current.Status.Phase == apiv2.ProjectPhaseReady, nil
	})
	requireReadyCondition(t, stillReady.Status.Conditions, metav1.ConditionTrue, apiv2.ProjectReasonPrepared)
	decorated.failEndpointLists.Store(false)
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.NoError(t, orchestrator.SimulateContainerStatus(ctx, prepared.Status.ContainerID, containers.ContainerStatusDead))
	failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
	decorated.reportContainerRunning.Store(true)
	decorated.failEndpointLists.Store(true)
	previousEndpointFailures := decorated.endpointListFailures.Load()
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return decorated.endpointListFailures.Load() > previousEndpointFailures &&
			current.Status.Phase == apiv2.ProjectPhaseFailed && current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
	})
	decorated.failEndpointLists.Store(false)
	stillFailed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseFailed && current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
	})
	require.Equal(t, failed.Status.Conditions[0].Message, stillFailed.Status.Conditions[0].Message)
	decorated.failContainerLists.Store(true)
	require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"project-terminal-lookup":"failed"}}}`))))
	lookupFailed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return decorated.containerListFailures.Load() != 0 && current.Status.Phase == apiv2.ProjectPhaseFailed, nil
	})
	require.Equal(t, failed.Status.Conditions[0].Reason, lookupFailed.Status.Conditions[0].Reason)
	require.Equal(t, failed.Status.Conditions[0].Message, lookupFailed.Status.Conditions[0].Message)
	decorated.failContainerLists.Store(false)
	decorated.reportContainerRunning.Store(false)
	decorated.failEndpointLists.Store(true)
	require.NoError(t, server.Client.Delete(ctx, resource))
	physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}}
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	var deleting apiv2.Project
	require.NoError(t, server.Client.Get(ctx, resource.NamespacedName(), &deleting))
	require.False(t, deleting.DeletionTimestamp.IsZero())
	require.Equal(t, apiv2.ProjectPhaseStopping, deleting.Status.Phase)
	decorated.failEndpointLists.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

func (apiClient *projectBackingClient) Get(ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption) error {
	if _, network := obj.(*apiv2.LogicalContainerNetwork); network && key.Name == "later" && apiClient.failNetwork.Load() {
		apiClient.networkLookups.Add(1)
		return errors.New("later network lookup unavailable")
	}
	return apiClient.Client.Get(ctx, key, obj, options...)
}

func (apiClient *projectBackingClient) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.PatchOption) error {
	if owner := metav1.GetControllerOf(obj); owner != nil && owner.Kind == "Project" &&
		!obj.GetDeletionTimestamp().IsZero() && !slices.Contains(obj.GetFinalizers(), controllers.LogicalBackingFinalizer) &&
		apiClient.loseHoldResponse.CompareAndSwap(true, false) {
		if patchErr := apiClient.Client.Patch(ctx, obj, patch, options...); patchErr != nil {
			return patchErr
		}
		apiClient.holdResponseLost.Store(true)
		return errors.New("Project hold release response lost")
	}
	return apiClient.Client.Patch(ctx, obj, patch, options...)
}

func (apiClient *projectBackingClient) Status() ctrl_client.SubResourceWriter {
	return &projectBackingStatusWriter{SubResourceWriter: apiClient.Client.Status(), client: apiClient}
}

type projectBackingStatusWriter struct {
	ctrl_client.SubResourceWriter
	client *projectBackingClient
}

type projectBackingReader struct {
	ctrl_client.Reader
	failLists              atomic.Bool
	endpointFailure        *atomic.Bool
	endpointFailures       *atomic.Int32
	createErrorPending     atomic.Bool
	immediateConfirmations atomic.Int32
}

func (reader *projectBackingReader) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, endpoints := list.(*apiv2.ServiceEndpointList); endpoints && reader.endpointFailure != nil && reader.endpointFailure.Load() {
		if reader.endpointFailures != nil {
			reader.endpointFailures.Add(1)
		}
		return errors.New("injected Project endpoint lookup failure")
	}
	if _, containers := list.(*apiv2.PhysicalContainerList); containers && reader.failLists.Load() {
		if reader.createErrorPending.Load() {
			reader.immediateConfirmations.Add(1)
		}
		return errors.New("uncached Project backing confirmation unavailable")
	}
	return reader.Reader.List(ctx, list, options...)
}

// Verifies that an ordinary container create error reports without immediate confirmation
// and lets later reconciliation adopt the committed backing without repeating creation.
func TestV2ProjectReconcilesAmbiguousCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var decorated *projectBackingClient
	var reader *projectBackingReader
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
				decorated = &projectBackingClient{Client: client}
				decorated.hideBackings.Store(true)
				return decorated
			},
			DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
				reader = &projectBackingReader{Reader: apiReader}
				decorated.confirmationReader = reader
				return reader
			},
		})
	require.NoError(t, startupErr)
	t.Cleanup(func() { reader.failLists.Store(false) })
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-create-confirmation")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhaseUnknown && len(current.Status.Conditions) != 0 &&
			current.Status.Conditions[0].Reason == string(apiv2.ProjectReasonPhysicalPreparationFailed), nil
	})
	require.Equal(t, int32(2), decorated.undecodedCreates.Load())
	require.Zero(t, reader.immediateConfirmations.Load())
	var backings apiv2.PhysicalContainerList
	require.NoError(t, server.Client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, backings.Items, 1)
	reader.failLists.Store(false)
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	require.Equal(t, int32(2), decorated.undecodedCreates.Load())
	require.Equal(t, backings.Items[0].UID, prepared.Status.RuntimeLogs.UID)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

func (writer *projectBackingStatusWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
	if _, project := obj.(*apiv2.Project); project && writer.client.confirmationReader != nil {
		writer.client.confirmationReader.createErrorPending.Store(false)
	}
	complete := false
	switch physical := obj.(type) {
	case *apiv2.PhysicalProcess:
		complete = writer.client.kind == "process" && physical.Status.LogsComplete
	case *apiv2.PhysicalContainerImage:
		complete = writer.client.kind == "image" && physical.Status.LogsComplete
	}
	if complete && !obj.GetDeletionTimestamp().IsZero() && writer.client.blockFinalStatus.Load() {
		writer.client.finalStatusOnce.Do(func() { writer.client.finalStatus <- obj.DeepCopyObject().(ctrl_client.Object) })
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "projectbackings"},
			obj.GetName(), errors.New("Project backing final status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

// Verifies that Project requests child deletion immediately, retains both finalizers through
// cleanup status conflicts, and retries a committed logical-hold release with a lost response.
func TestV2ProjectDeletionRetainsBackingUntilCleanupIsDurable(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"image"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectBackingClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectBackingClient{Client: apiClient, kind: kind, finalStatus: make(chan ctrl_client.Object, 1)}
					return decorated
				}})
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-durable-"+kind)
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir()}}
			var backing ctrl_client.Object = &apiv2.PhysicalProcess{}
			cleanupFinalizer := apiv2.GroupName + "/physicalprocess-reconciler"
			if kind == "process" {
				resource.Spec.Process = &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}
			} else {
				orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
				source := namespace.Name + "-image"
				releasePull := sync.OnceFunc(orchestrator.BlockPullImage(source))
				t.Cleanup(releasePull)
				resource.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source},
					Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing"}}},
				}
				backing = &apiv2.PhysicalContainerImage{}
				cleanupFinalizer = apiv2.GroupName + "/physicalcontainerimage-reconciler"
			}
			require.NoError(t, server.Client.Create(ctx, resource))
			if kind == "process" {
				waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			} else {
				waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.StartupLogs != nil && current.Status.StartupLogs.Available && !current.Status.StartupLogs.Complete, nil
				})
			}
			name := types.NamespacedName{Namespace: namespace.Name, Name: "project-" + string(resource.UID)}
			require.NoError(t, server.Client.Get(ctx, name, backing))
			require.Contains(t, backing.GetFinalizers(), controllers.LogicalBackingFinalizer)
			require.Contains(t, backing.GetFinalizers(), cleanupFinalizer)
			decorated.blockFinalStatus.Store(true)
			t.Cleanup(func() { decorated.blockFinalStatus.Store(false) })
			decorated.loseHoldResponse.Store(true)
			require.NoError(t, server.Client.Delete(ctx, resource))
			select {
			case <-decorated.finalStatus:
			case <-ctx.Done():
				t.Fatal("Project backing cleanup did not publish its disposition")
			}
			require.NoError(t, server.Client.Get(ctx, name, backing))
			require.False(t, backing.GetDeletionTimestamp().IsZero())
			require.Contains(t, backing.GetFinalizers(), cleanupFinalizer)
			require.Contains(t, backing.GetFinalizers(), controllers.LogicalBackingFinalizer)
			require.NoError(t, server.Client.Get(ctx, resource.NamespacedName(), &apiv2.Project{}))
			decorated.blockFinalStatus.Store(false)
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			waitProjectBackingDeleted(t, ctx, server.Client, backing)
			require.True(t, decorated.holdResponseLost.Load())
		})
	}
}

// Verifies that Project confirms undecoded creates by owner through uncached reads and deletes
// every owned backing, including duplicates with unexpected names and no recorded UID.
func TestV2ProjectDiscoversAndDeletesBackingsByOwner(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectBackingClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectBackingClient{Client: apiClient}
					decorated.hideBackings.Store(true)
					return decorated
				}})
			require.NoError(t, startupErr)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-owner-"+kind)
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir()}}
			if kind == "process" {
				resource.Spec.Process = &apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace.Name + "-command"}}
			} else {
				resource.Spec.Container = &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}}}
			}
			require.NoError(t, server.Client.Create(ctx, resource))
			running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			expectedCreates := int32(1)
			if kind == "container" {
				expectedCreates = 2
			}
			require.Equal(t, expectedCreates, decorated.undecodedCreates.Load())
			var original ctrl_client.Object = &apiv2.PhysicalProcess{}
			if kind == "container" {
				original = &apiv2.PhysicalContainer{}
			}
			require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.RuntimeLogs.Name}, original))
			duplicate := original.DeepCopyObject().(ctrl_client.Object)
			duplicate.SetName("unexpected-owned-backing")
			duplicate.SetUID("")
			duplicate.SetResourceVersion("")
			duplicate.SetManagedFields(nil)
			require.NoError(t, server.Client.Create(ctx, duplicate))
			// A second owned resource invalidates normal observation, but must not block cleanup.
			unknown := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseUnknown)
			requireReadyCondition(t, unknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPhysicalExecutionChanged)
			require.NoError(t, server.Client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			stillUnknown := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				return current.Status.Phase == apiv2.ProjectPhaseUnknown && current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
			})
			requireReadyCondition(t, stillUnknown.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPhysicalExecutionChanged)
			require.Equal(t, running.Status.RuntimeLogs.UID, stillUnknown.Status.RuntimeLogs.UID)
			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
			waitProjectBackingDeleted(t, ctx, server.Client, original)
			waitProjectBackingDeleted(t, ctx, server.Client, duplicate)
		})
	}
}

func waitProjectBackingDeleted(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, backing ctrl_client.Object) {
	t.Helper()
	switch physical := backing.(type) {
	case *apiv2.PhysicalProcess:
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
	case *apiv2.PhysicalContainer:
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
	case *apiv2.PhysicalContainerImage:
		ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
	default:
		t.Fatalf("unsupported Project backing %T", backing)
	}
}

// Verifies that an empty cached owner list cannot finalize Project deletion when uncached
// confirmation fails, and that reconciliation retries cleanup once confirmation is available.
func TestV2ProjectDeletionRetriesUncachedOwnerConfirmation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var reader *projectBackingReader
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{
			DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
				decorated := &projectBackingClient{Client: apiClient}
				decorated.hideBackings.Store(true)
				return decorated
			},
			DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
				reader = &projectBackingReader{Reader: apiReader}
				return reader
			},
		})
	require.NoError(t, startupErr)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-delete-confirmation")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	reader.failLists.Store(true)
	t.Cleanup(func() { reader.failLists.Store(false) })
	require.NoError(t, server.Client.Delete(ctx, resource))
	failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseUnknown)
	requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "PhysicalDeletionFailed")
	require.Contains(t, failed.Finalizers, apiv2.GroupName+"/project-reconciler")
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.True(t, physical.DeletionTimestamp.IsZero())
	reader.failLists.Store(false)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
}

// Verifies that Project evaluates later network failures and lookup errors despite an earlier
// missing reference, and delegates readiness after a logical network publishes its backing reference.
func TestV2ProjectChecksAllNetworkReferences(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"failed", "lookup", "published"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectBackingClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectBackingClient{Client: apiClient}
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
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{ImageRef: "external-image", Networks: networks}}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			switch scenario {
			case "failed":
				failed := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
				requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, "NetworkFailed")
				require.Contains(t, failed.Status.Conditions[0].Message, "later failed")
			case "lookup":
				unknown := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseUnknown)
				requireReadyCondition(t, unknown.Status.Conditions, metav1.ConditionFalse, "NetworkLookupFailed")
				require.Positive(t, decorated.networkLookups.Load())
				decorated.failNetwork.Store(false)
				waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
			case "published":
				pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
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
