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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type projectBackingClient struct {
	ctrl_client.Client
	kind             string
	blockFinalStatus atomic.Bool
	finalStatus      chan ctrl_client.Object
	finalStatusOnce  sync.Once
	loseHoldResponse atomic.Bool
	holdResponseLost atomic.Bool
	hideBackings     atomic.Bool
	undecodedCreates atomic.Int32
	failNetwork      atomic.Bool
	networkLookups   atomic.Int32
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
	return errors.New("Project backing create committed without a decoded response")
}

func (apiClient *projectBackingClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if listErr := apiClient.Client.List(ctx, list, options...); listErr != nil {
		return listErr
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
	failLists atomic.Bool
}

func (reader *projectBackingReader) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, containers := list.(*apiv2.PhysicalContainerList); containers && reader.failLists.Load() {
		return errors.New("uncached Project backing confirmation unavailable")
	}
	return reader.Reader.List(ctx, list, options...)
}

func (writer *projectBackingStatusWriter) Patch(ctx context.Context, obj ctrl_client.Object, patch ctrl_client.Patch, options ...ctrl_client.SubResourcePatchOption) error {
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
			waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseFailed)
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
