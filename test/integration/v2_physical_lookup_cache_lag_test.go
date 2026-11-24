/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// ownerCacheLagClient withholds owned physical resources from owner-index lists, simulating a cache that has
// not yet observed successfully created children.
type ownerCacheLagClient struct {
	ctrl_client.Client
	ownerField string
}

func (apiClient *ownerCacheLagClient) List(ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption) error {
	if listErr := apiClient.Client.List(ctx, list, options...); listErr != nil {
		return listErr
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if listOptions.FieldSelector == nil || !strings.Contains(listOptions.FieldSelector.String(), apiClient.ownerField) {
		return nil
	}
	switch children := list.(type) {
	case *apiv2.PhysicalContainerList:
		children.Items = nil
	case *apiv2.PhysicalContainerImageList:
		children.Items = nil
	case *apiv2.PhysicalContainerNetworkList:
		children.Items = nil
	}
	return nil
}

func startOwnerCacheLagEnvironment(t *testing.T, ctx context.Context, controllers IncludedController, ownerField string) ctrl_client.Client {
	t.Helper()
	server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, controllers, t.Name(), t.TempDir(), TestEnvironmentOptions{
		DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
			return &ownerCacheLagClient{Client: apiClient, ownerField: ownerField}
		},
	})
	require.NoError(t, startupErr)
	return server.Client
}

// Verifies that LogicalContainer resolves created physical resources the cache has not observed, so startup
// completes and a later stop reaches the physical container instead of reporting it missing.
func TestV2LogicalContainerResolvesPhysicalResourcesMissingFromCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	apiClient := startOwnerCacheLagEnvironment(t, ctx,
		NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
		".metadata.logicalContainerOwnerUID")
	namespace := createV2ExecNamespace(t, ctx, apiClient, "v2-logical-container-cache-lag")
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
		Spec: apiv2.LogicalContainerSpec{
			Container: &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		},
	}
	require.NoError(t, apiClient.Create(ctx, logical))
	running := waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
	})
	require.NoError(t, apiClient.Patch(ctx, logical, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	waitObjectAssumesStateEx(t, ctx, apiClient, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
		require.NotEqual(t, apiv2.LogicalContainerPhaseFailed, current.Status.Phase)
		return current.Status.Phase == apiv2.LogicalContainerPhaseExited, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.True(t, physical.Spec.Stop)
	require.NoError(t, apiClient.Delete(ctx, logical))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, logical)
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
}

// Verifies that LogicalContainerNetwork resolves a created physical network the cache has not observed,
// becoming Ready instead of reporting the physical network missing.
func TestV2LogicalContainerNetworkResolvesPhysicalNetworkMissingFromCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	apiClient := startOwnerCacheLagEnvironment(t, ctx,
		NamespaceController|LogicalContainerNetworkController|PhysicalContainerNetworkController,
		".metadata.logicalNetworkOwnerUID")
	namespace := createV2ExecNamespace(t, ctx, apiClient, "v2-logical-network-cache-lag")
	network := &apiv2.LogicalContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: namespace.Name},
		Spec:       apiv2.LogicalContainerNetworkSpec{Network: &apiv2.LogicalContainerNetworkConfig{}},
	}
	require.NoError(t, apiClient.Create(ctx, network))
	ready := waitObjectAssumesStateEx(t, ctx, apiClient, network.NamespacedName(), func(current *apiv2.LogicalContainerNetwork) (bool, error) {
		require.NotEqual(t, apiv2.LogicalContainerNetworkPhaseFailed, current.Status.Phase)
		return current.Status.Phase == apiv2.LogicalContainerNetworkPhaseReady, nil
	})
	physical := &apiv2.PhysicalContainerNetwork{}
	require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: ready.Status.PhysicalNetworkRef}, physical))
	require.NoError(t, apiClient.Delete(ctx, network))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, network)
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
}

// Verifies that ProjectContainer resolves created physical resources the cache has not observed, so a direct
// application runs and a later stop reaches the physical container instead of reporting it missing.
func TestV2ProjectContainerResolvesPhysicalResourcesMissingFromCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	apiClient := startOwnerCacheLagEnvironment(t, ctx, projectTestControllers, ".metadata.projectContainerOwnerUID")
	namespace := createV2ExecNamespace(t, ctx, apiClient, "v2-project-container-cache-lag")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{
			ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), LaunchMode: apiv2.ProjectLaunchModeDirect},
			Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Entrypoint: "application",
			}},
			DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}},
		}}
	require.NoError(t, apiClient.Create(ctx, resource))
	running := waitProjectPhase(t, ctx, apiClient, resource, apiv2.ProjectPhaseActive)
	require.NoError(t, apiClient.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
	waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		require.NotEqual(t, apiv2.ProjectPhaseFailed, current.Status.Phase)
		return current.Status.Phase == apiv2.ProjectPhaseStopped, nil
	})
	physical := &apiv2.PhysicalContainer{}
	require.NoError(t, apiClient.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
	require.True(t, physical.Spec.Stop)
	require.NoError(t, apiClient.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, apiClient, physical)
}
