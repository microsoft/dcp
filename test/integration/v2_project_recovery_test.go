/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"io"
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
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

const projectTestControllers = ProjectController | PhysicalProcessController | PhysicalContainerController | PhysicalContainerImageController

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
	if owner == nil || owner.Kind != "Project" || (apiClient.kind == "image" && !image) ||
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

// Verifies that Project validates exact-name AlreadyExists collisions for physical containers
// and images, accepting owned container declarations and rejecting incompatible identities or image inputs.
func TestV2ProjectValidatesPhysicalCreateCollisions(t *testing.T) {
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
				resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
					Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
						LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
					}}}
				require.NoError(t, server.Client.Create(ctx, resource))
				expected := apiv2.ProjectPhaseFailed
				if scenario == "compatible" || (kind == "container" && scenario == "spec") {
					expected = apiv2.ProjectPhaseReady
				}
				observed := waitProjectPhase(t, ctx, server.Client, resource, expected)
				if expected == apiv2.ProjectPhaseFailed {
					reason := apiv2.ProjectReasonPhysicalPreparationFailed
					if kind == "image" {
						reason = apiv2.ProjectReasonImagePreparationFailed
					}
					requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, reason)
				}
				if scenario == "hold" || scenario == "owner" {
					var collision ctrl_client.Object = &apiv2.PhysicalContainer{}
					if kind == "image" {
						collision = &apiv2.PhysicalContainerImage{}
					}
					require.NoError(t, server.Client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: "project-" + string(resource.UID)}, collision))
					if scenario == "hold" {
						collision.SetFinalizers(nil)
						require.NoError(t, server.Client.Update(ctx, collision))
					} else {
						removeProjectBackingHold(t, ctx, server.Client, collision)
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
		owner != nil && owner.Kind == "Project" && apiClient.responseLost.CompareAndSwap(false, true) {
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
func TestV2ProjectRecoversLostCreateAndMutableStop(t *testing.T) {
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
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
					LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Command: []string{"application"}},
				}}}
			require.NoError(t, server.Client.Create(ctx, resource))
			var backing ctrl_client.Object
			select {
			case backing = <-decorated.created:
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
			physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: backing.GetName(), Namespace: namespace.Name}}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
			image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: backing.GetName(), Namespace: namespace.Name}}
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, image)
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.LessOrEqual(t, orchestrator.CreateContainerCallCount(backing.GetName()), 1)
		})
	}
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
	if resource, project := obj.(*apiv2.Project); project &&
		resource.Status.Phase == apiv2.ProjectPhaseFailed &&
		resource.Status.RuntimeLogs != nil && resource.Status.RuntimeLogs.Complete && !resource.Status.RuntimeLogs.Available &&
		writer.client.conflicted.CompareAndSwap(false, true) {
		return apierrors.NewConflict(schema.GroupResource{Group: apiv2.GroupName, Resource: "projects"}, resource.Name, errors.New("injected Project status conflict"))
	}
	return writer.SubResourceWriter.Patch(ctx, obj, patch, options...)
}

// Verifies that forced disappearance of a prepared container retains either its observed termination
// failure or missing-context outcome, preserves capture identity through conflicts, and completes followers.
func TestV2ProjectMissingBackingCompletesFollowerThroughConflict(t *testing.T) {
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
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	prepared := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
	require.NoError(t, orchestrator.SimulateContainerLogging(prepared.Status.ContainerID, apiv1.LogStreamSourceStdout, []byte("before-delete\n")))
	follow, followErr := server.RestClient.Get().
		AbsPath("/apis", apiv2.GroupName, apiv2.Version, "namespaces", namespace.Name, "projects", resource.Name, "log").
		Param("follow", "true").Stream(ctx)
	require.NoError(t, followErr)
	defer follow.Close()
	initial := make([]byte, len("before-delete\n"))
	_, readErr := io.ReadFull(follow, initial)
	require.NoError(t, readErr)
	physical := &apiv2.PhysicalContainer{ObjectMeta: metav1.ObjectMeta{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}}
	require.NoError(t, server.Client.Delete(ctx, physical))
	removeProjectBackingHold(t, ctx, server.Client, physical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, physical)
	failed := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		if ready == nil {
			return false, nil
		}
		terminal := current.Status.Phase == apiv2.ProjectPhaseFailed &&
			(ready.Reason == string(apiv2.ProjectReasonContainerContextMissing) || ready.Reason == string(apiv2.ProjectReasonContainerContextTerminating))
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

func removeProjectBackingHold(t *testing.T, ctx context.Context, apiClient ctrl_client.Client, backing ctrl_client.Object) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if getErr := apiClient.Get(ctx, ctrl_client.ObjectKeyFromObject(backing), backing); getErr != nil {
			return getErr
		}
		return controllers.ReleaseLogicalBackingFinalizer(ctx, apiClient, backing)
	}))
}
