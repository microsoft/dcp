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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Project prepares and publishes its image capture before waiting for networks,
// but does not publish an inline image reference while its namespace prevents creation.
func TestV2ProjectPreparesImageBeforeNetworks(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-image-before-network"},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "v2-project-image-before-network-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing-network"}}},
		}}}
	require.NoError(t, client.Create(ctx, resource))
	pending := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	require.Empty(t, pending.Status.ImageRef)
	require.Nil(t, pending.Status.StartupLogs)
	createActiveV2Namespace(t, ctx, resource.Namespace)
	image := waitPhysicalContainerImagePhase(t, ctx, types.NamespacedName{
		Namespace: resource.Namespace, Name: "project-" + string(resource.UID),
	}, apiv2.PhysicalContainerImagePhaseReady)
	prepared := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == "NetworkNotReady" && current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
	})
	require.Equal(t, image.Name, prepared.Status.ImageRef)
	require.Equal(t, image.GetLogSource(), prepared.Status.StartupLogs)
	require.Empty(t, prepared.Status.PhysicalContainerRef)
	require.Equal(t, 1, containerOrchestrator.PullImageCallCount(resource.Spec.Container.Image.Image))
	require.Zero(t, containerOrchestrator.CreateContainerCallCount(image.Name))
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
}

// Verifies that stopping Project before preparation publishes no unrealized inline image reference,
// and stopping after image preparation still observes image capture completion without launching execution.
func TestV2ProjectStopPreservesPreparedImageLifecycle(t *testing.T) {
	t.Parallel()
	for _, prepare := range []bool{false, true} {
		name := "unprepared"
		if prepare {
			name = "prepared"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-project-stop-image-"+name)
			source := namespace.Name + "-image"
			releasePull := sync.OnceFunc(containerOrchestrator.BlockPullImage(source))
			t.Cleanup(releasePull)
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Stop: !prepare, Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source},
					Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing-network"}}},
				}}}
			require.NoError(t, client.Create(ctx, resource))
			if prepare {
				waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.StartupLogs != nil && current.Status.StartupLogs.Available && !current.Status.StartupLogs.Complete, nil
				})
				require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			}
			stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
			require.Empty(t, stopped.Status.PhysicalContainerRef)
			if prepare {
				require.NotEmpty(t, stopped.Status.ImageRef)
				releasePull()
				waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseStopped && current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
				})
			} else {
				require.Empty(t, stopped.Status.ImageRef)
				require.Nil(t, stopped.Status.StartupLogs)
				require.Zero(t, containerOrchestrator.PullImageCallCount(source))
			}
		})
	}
}

type projectRejectedCreateClient struct {
	ctrl_client.Client
	rejected     atomic.Bool
	attempts     atomic.Int32
	entered      chan struct{}
	release      chan struct{}
	retryRelease chan struct{}
}

func (apiClient *projectRejectedCreateClient) Create(ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	owner := metav1.GetControllerOf(obj)
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && owner != nil && owner.Kind == "Project" {
		apiClient.attempts.Add(1)
		if apiClient.rejected.CompareAndSwap(false, true) {
			close(apiClient.entered)
			select {
			case <-apiClient.release:
			case <-ctx.Done():
			}
			return errors.New("injected rejection before physical creation")
		}
		if apiClient.retryRelease != nil {
			select {
			case <-apiClient.retryRelease:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return apiClient.Client.Create(ctx, obj, options...)
}

// Verifies that a rejected container declaration leaves Project Pending rather than inventing
// observation uncertainty, and retries the same prepared inputs to realize one backing.
func TestV2ProjectRejectedCreatePreservesPreparationState(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var rejectedCreate *projectRejectedCreateClient
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(client ctrl_client.Client) ctrl_client.Client {
			rejectedCreate = &projectRejectedCreateClient{Client: client, entered: make(chan struct{}), release: make(chan struct{}), retryRelease: make(chan struct{})}
			return rejectedCreate
		}})
	require.NoError(t, startupErr)
	release := sync.OnceFunc(func() { close(rejectedCreate.release) })
	t.Cleanup(release)
	releaseRetry := sync.OnceFunc(func() { close(rejectedCreate.retryRelease) })
	t.Cleanup(releaseRetry)
	namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-rejected-preparation")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	select {
	case <-rejectedCreate.entered:
	case <-ctx.Done():
		t.Fatal("Project did not attempt backing creation")
	}
	release()
	pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.Phase == apiv2.ProjectPhasePending && current.Status.PhysicalContainerRef == "" &&
			len(current.Status.Conditions) != 0, nil
	})
	require.Equal(t, apiv2.ProjectPhasePending, pending.Status.Phase)
	require.Empty(t, pending.Status.PhysicalContainerRef)
	releaseRetry()
	waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
	require.EqualValues(t, 2, rejectedCreate.attempts.Load())
	var backings apiv2.PhysicalContainerList
	require.NoError(t, server.Client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
	require.Len(t, backings.Items, 1)
	require.NoError(t, server.Client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
}

// Verifies that Project resolves network references again after a rejected declaration and uses
// the current published backing when no container declaration committed the earlier selection.
func TestV2ProjectResolvesNetworksAfterRejectedCreate(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	var rejectedCreate *projectRejectedCreateClient
	serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx,
		projectTestControllers|LogicalContainerNetworkController|PhysicalContainerNetworkController, t.Name(), t.TempDir(),
		TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			rejectedCreate = &projectRejectedCreateClient{Client: apiClient, entered: make(chan struct{}), release: make(chan struct{})}
			return rejectedCreate
		}})
	require.NoError(t, startupErr)
	releaseCreate := sync.OnceFunc(func() { close(rejectedCreate.release) })
	t.Cleanup(releaseCreate)
	apiClient := serverInfo.Client
	namespace := "v2-project-rejected-network"
	createV2ExecNamespace(t, ctx, apiClient, namespace)
	network := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace},
		Spec: apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}}}
	require.NoError(t, apiClient.Create(ctx, network))
	waitObjectAssumesStateEx(t, ctx, apiClient, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: network.Name}}},
		}}}
	require.NoError(t, apiClient.Create(ctx, resource))
	select {
	case <-rejectedCreate.entered:
	case <-ctx.Done():
		t.Fatal("Project did not attempt physical creation")
	}
	require.NoError(t, apiClient.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, network)
	replacement := &apiv2.LogicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: network.Name, Namespace: namespace}, Spec: *network.Spec.DeepCopy()}
	require.NoError(t, apiClient.Create(ctx, replacement))
	replaced := waitObjectAssumesStateEx(t, ctx, apiClient, replacement.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	releaseCreate()
	waitProjectPhase(t, ctx, apiClient, resource, apiv2.ProjectPhaseReady)
	require.EqualValues(t, 2, rejectedCreate.attempts.Load())
	var physical apiv2.PhysicalContainerList
	require.NoError(t, apiClient.List(ctx, &physical, ctrl_client.InNamespace(namespace)))
	require.Len(t, physical.Items, 1)
	require.Equal(t, replaced.Status.PhysicalNetworkRef, physical.Items[0].Spec.Container.Networks[0].NetworkRef)
	require.NoError(t, apiClient.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
}

type projectImageFailureReader struct {
	ctrl_client.Reader
	enabled  atomic.Bool
	failures atomic.Int32
}

func (reader *projectImageFailureReader) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if _, image := objects.(*apiv2.PhysicalContainerImageList); image && reader.enabled.Load() {
		reader.failures.Add(1)
		return errors.New("injected Project startup-log lookup failure")
	}
	return reader.Reader.List(ctx, objects, options...)
}

type projectImageObservationClient struct {
	ctrl_client.Client
	reader *projectImageFailureReader
}

func (apiClient *projectImageObservationClient) List(ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	return apiClient.reader.List(ctx, objects, options...)
}

// Verifies that startup-log lookup errors preserve Ready or terminal preparation failure
// and the previously observed image capture, without requiring terminal observation to poll.
func TestV2ProjectImageLogFailurePreservesExecutionState(t *testing.T) {
	t.Parallel()
	for _, phase := range []apiv2.ProjectPhase{apiv2.ProjectPhaseReady, apiv2.ProjectPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var imageReader *projectImageFailureReader
			serverInfo, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					imageReader = &projectImageFailureReader{Reader: apiClient}
					return &projectImageObservationClient{Client: apiClient, reader: imageReader}
				}})
			require.NoError(t, startupErr)
			apiClient := serverInfo.Client
			namespace := "v2-project-log-state-" + strings.ToLower(string(phase))
			createV2ExecNamespace(t, ctx, apiClient, namespace)
			orchestrator, simulated := serverInfo.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.True(t, simulated)
			resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace},
				Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"}}}}}
			if phase == apiv2.ProjectPhaseFailed {
				orchestrator.FailNextPullImage(resource.Spec.Container.Image.Image, errors.New("injected terminal image failure"))
				noRetries := int32(0)
				resource.Spec.Container.Image.PullRetryLimit = &noRetries
			}

			require.NoError(t, apiClient.Create(ctx, resource))
			initial := waitProjectPhase(t, ctx, apiClient, resource, phase)
			require.NotNil(t, initial.Status.StartupLogs)
			imageReader.enabled.Store(true)
			require.NoError(t, apiClient.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"project-log-probe":"enabled"}}}`))))
			observed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
				return imageReader.failures.Load() >= 1 && current.Status.Phase == phase, nil
			})
			require.Equal(t, initial.Status.StartupLogs, observed.Status.StartupLogs)
			imageReader.enabled.Store(false)
		})
	}
}

// Verifies that an owned image's logical hold preserves startup capture for late Project followers
// after physical image cleanup, and Project deletion releases the hold without recreating the image.
func TestV2ProjectOwnedImageDeletionPreservesLateStartupLogs(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-held-startup")
	resource := &apiv2.Project{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectSpec{RootDirectory: t.TempDir(), Container: &apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing"}}},
		}}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.Project) (bool, error) {
		return current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
	})
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: prepared.Status.ImageRef, Namespace: namespace.Name}}
	require.NoError(t, client.Delete(ctx, image))
	waitObjectAssumesState(t, ctx, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return !current.DeletionTimestamp.IsZero() &&
			!slices.Contains(current.Finalizers, apiv2.GroupName+"/physicalcontainerimage-reconciler"), nil
	})
	failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
	require.Equal(t, prepared.Status.StartupLogs.UID, failed.Status.StartupLogs.UID)
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true})
	require.NoError(t, outputErr)
	require.Contains(t, output, "Pulling image "+namespace.Name+"-image")
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
}
