/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strconv"
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
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that ProjectContainer prepares and publishes its image capture before waiting for networks,
// but does not publish an inline image reference while its namespace prevents creation.
func TestV2ProjectContainerPreparesImageBeforeNetworks(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: "v2-project-image-before-network"},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "v2-project-image-before-network-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing-network"}}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, resource))
	pending := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhasePending)
	require.Empty(t, pending.Status.ImageRef)
	require.Nil(t, pending.Status.StartupLogs)
	createActiveV2Namespace(t, ctx, resource.Namespace)
	image := waitPhysicalContainerImagePhase(t, ctx, types.NamespacedName{
		Namespace: resource.Namespace, Name: "project-" + string(resource.UID),
	}, apiv2.PhysicalContainerImagePhaseReady)
	prepared := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		ready := apimeta.FindStatusCondition(current.Status.Conditions, string(apiv2.ConditionReady))
		return ready != nil && ready.Reason == string(apiv2.ProjectReasonPreparing) &&
			current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
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

// Verifies that stopping ProjectContainer before preparation publishes no unrealized inline image reference,
// and stopping after image preparation still observes image capture completion without launching execution.
func TestV2ProjectContainerStopPreservesPreparedImageLifecycle(t *testing.T) {
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
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir(), Stop: !prepare}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: source},
					Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing-network"}}},
				}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			require.NoError(t, client.Create(ctx, resource))
			if prepare {
				waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return current.Status.StartupLogs != nil && current.Status.StartupLogs.Available && !current.Status.StartupLogs.Complete, nil
				})
				require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			}
			stopped := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
			require.Empty(t, stopped.Status.PhysicalContainerRef)
			if prepare {
				require.NotEmpty(t, stopped.Status.ImageRef)
				releasePull()
				waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
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
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && owner != nil && owner.Kind == "ProjectContainer" {
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

// Verifies that a rejected container declaration leaves ProjectContainer Pending rather than inventing
// observation uncertainty, and retries the same prepared inputs to realize one backing.
func TestV2ProjectContainerRejectedCreatePreservesPreparationState(t *testing.T) {
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
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{
			LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, server.Client.Create(ctx, resource))
	select {
	case <-rejectedCreate.entered:
	case <-ctx.Done():
		t.Fatal("Project did not attempt backing creation")
	}
	release()
	pending := waitObjectAssumesStateEx(t, ctx, server.Client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
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

// Verifies that ProjectContainer resolves network references again after a rejected declaration and uses
// the current published backing when no container declaration committed the earlier selection.
func TestV2ProjectContainerResolvesNetworksAfterRejectedCreate(t *testing.T) {
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
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: network.Name}}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
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
func TestV2ProjectContainerImageLogFailurePreservesExecutionState(t *testing.T) {
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
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"}}}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
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
			observed := waitObjectAssumesStateEx(t, ctx, apiClient, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				return imageReader.failures.Load() >= 1 && current.Status.Phase == phase, nil
			})
			require.Equal(t, initial.Status.StartupLogs, observed.Status.StartupLogs)
			imageReader.enabled.Store(false)
		})
	}
}

// Verifies that an owned image's logical hold preserves startup capture for late ProjectContainer followers
// after physical image cleanup without failing the Project, and ProjectContainer deletion releases the hold
// without recreating the image.
func TestV2ProjectContainerOwnedImageDeletionPreservesLateStartupLogs(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	t.Cleanup(cancel)
	namespace := createActiveV2Namespace(t, ctx, "v2-project-held-startup")
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
		Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
			Networks: []apiv2.LogicalContainerNetworkConnectionConfig{{NetworkRef: "missing"}}},
		}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
	require.NoError(t, client.Create(ctx, resource))
	prepared := waitObjectAssumesState(t, ctx, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
		return current.Status.StartupLogs != nil && current.Status.StartupLogs.Complete, nil
	})
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: prepared.Status.ImageRef, Namespace: namespace.Name}}
	require.NoError(t, client.Delete(ctx, image))
	waitObjectAssumesState(t, ctx, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
		return !current.DeletionTimestamp.IsZero() &&
			!slices.Contains(current.Finalizers, apiv2.GroupName+"/physicalcontainerimage-reconciler"), nil
	})
	output, outputErr := readV2ContainerLog(ctx, resource, apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true})
	require.NoError(t, outputErr)
	require.Contains(t, output, "Pulling image "+namespace.Name+"-image")
	// Like LogicalContainer, a deletion request on a held owned image does not fail the Project.
	current := &apiv2.ProjectContainer{}
	require.NoError(t, client.Get(ctx, resource.NamespacedName(), current))
	require.Equal(t, apiv2.ProjectPhasePending, current.Status.Phase)
	require.Equal(t, prepared.Status.StartupLogs.UID, current.Status.StartupLogs.UID)
	require.NoError(t, client.Delete(ctx, resource))
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
	ctrl_testutil.WaitObjectDeleted(t, ctx, client, image)
}

// Verifies that ProjectProcess rejects terminating service dependencies during port or template preparation
// and preserves that terminal failure after the dependency is replaced and Stop is requested.
func TestV2ProjectProcessTerminalServicePreparation(t *testing.T) {
	t.Parallel()
	kind := "process"

	for _, source := range []string{"port", "template"} {
		t.Run(kind+"/"+source, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-project-terminal-service-"+kind+"-"+source)
			service, releaseService := createTerminatingPreparationService(t, ctx, namespace.Name)
			resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}
			arguments := []string{`{{portFor "dependency"}}`}
			binding := apiv2.LogicalServiceBinding{Name: "application", ServiceRef: service.Name}
			if source == "port" {
				arguments = []string{"literal"}
			}

			resource.Spec.Process = apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{
				ExecutablePath: "application", Args: arguments,
				AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit},
			}}
			if source == "port" {
				resource.Spec.Process.Ports = []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Port: 8080}}
			}

			require.NoError(t, client.Create(ctx, resource))
			failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
			releaseService()
			replacement := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: service.Name, Namespace: namespace.Name}, Spec: service.Spec}
			require.NoError(t, client.Create(ctx, replacement))
			require.NotEqual(t, service.UID, replacement.UID)
			require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectProcess) error {
				current.Status.Phase, current.Status.Conditions = apiv2.ProjectPhaseUnknown, nil
				return client.Status().Update(updateCtx, current)
			}))
			terminal := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, terminal.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
			require.Equal(t, failed.Status.Conditions[0].Message, terminal.Status.Conditions[0].Message)
			var backings apiv2.PhysicalContainerList
			require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, backings.Items)
			require.NoError(t, client.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
		})
	}

}

// Verifies that ProjectContainer rejects terminating service dependencies during port or template preparation
// and preserves that terminal failure after the dependency is replaced and Stop is requested.
func TestV2ProjectContainerTerminalServicePreparation(t *testing.T) {
	t.Parallel()
	kind := "container"

	for _, source := range []string{"port", "template"} {
		t.Run(kind+"/"+source, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			namespace := createActiveV2Namespace(t, ctx, "v2-project-terminal-service-"+kind+"-"+source)
			service, releaseService := createTerminatingPreparationService(t, ctx, namespace.Name)
			resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter"}}}}}
			arguments := []string{`{{portFor "dependency"}}`}
			binding := apiv2.LogicalServiceBinding{Name: "application", ServiceRef: service.Name}
			if source == "port" {
				arguments = []string{"literal"}
			}

			resource.Spec.Container = apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"}, Command: arguments,
			}}
			if source == "port" {
				resource.Spec.Container.Ports = []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080}}
			}

			require.NoError(t, client.Create(ctx, resource))
			failed := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
			require.Empty(t, failed.Status.PhysicalContainerRef)
			releaseService()
			replacement := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: service.Name, Namespace: namespace.Name}, Spec: service.Spec}
			require.NoError(t, client.Create(ctx, replacement))
			require.NotEqual(t, service.UID, replacement.UID)
			require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
			require.NoError(t, retryOnConflict(ctx, resource.NamespacedName(), func(updateCtx context.Context, current *apiv2.ProjectContainer) error {
				current.Status.Phase, current.Status.Conditions = apiv2.ProjectPhaseUnknown, nil
				return client.Status().Update(updateCtx, current)
			}))
			terminal := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseFailed)
			requireReadyCondition(t, terminal.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
			require.Equal(t, failed.Status.Conditions[0].Message, terminal.Status.Conditions[0].Message)
			var backings apiv2.PhysicalContainerList
			require.NoError(t, client.List(ctx, &backings, ctrl_client.InNamespace(namespace.Name)))
			require.Empty(t, backings.Items)
			require.NoError(t, client.Delete(ctx, namespace))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, namespace)
		})
	}

}

// Verifies that container TCP adapter ports are declared before creation, publish their runtime-assigned loopback
// binding through ProjectContainer once the adapter container starts, and are rejected on TCP application-port conflicts.
func TestV2ProjectContainerAdapterTCPPreparation(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"default", "zero", "explicit", "tcp-conflict", "udp-coexist"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "project-tcp-port-"+scenario)
			project := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
					Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				}},
					DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: t.TempDir(), Args: []string{"adapter", "--port={{port}}"},
						Mode: apiv2.DebugAdapterModeTCPConnect}},
				}}
			expectedPort := int32(4711)
			if scenario == "zero" {
				project.Spec.DebugAdapter.TCP = &apiv2.DebugAdapterTCPConfig{}
			} else if scenario == "explicit" {
				expectedPort = 5678
				project.Spec.DebugAdapter.TCP = &apiv2.DebugAdapterTCPConfig{Port: expectedPort}
			}
			if scenario == "tcp-conflict" || scenario == "udp-coexist" {
				protocol := commonapi.PortProtocolTCP
				if scenario == "udp-coexist" {
					protocol = commonapi.PortProtocolUDP
				}
				project.Spec.Container.Ports = []apiv2.LogicalContainerPort{{ContainerPort: expectedPort,
					LogicalServiceBinding: apiv2.LogicalServiceBinding{Name: "application", Service: &apiv2.LogicalServiceTemplate{
						Name: "application", LogicalServiceSpec: apiv2.LogicalServiceSpec{Protocol: protocol},
					}}}}
			}
			require.NoError(t, client.Create(ctx, project))
			if scenario == "tcp-conflict" {
				failed := waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseFailed)
				requireReadyCondition(t, failed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
				require.Contains(t, failed.Status.Conditions[0].Message, "adapter port conflicts")
				require.Empty(t, failed.Status.PhysicalContainerRef)
			} else {
				prepared := waitProjectPhase(t, ctx, client, project, apiv2.ProjectPhaseReady)
				require.Equal(t, expectedPort, prepared.Status.DebugAdapterPort)
				// Runtimes assign host bindings at start, which the first debug session performs.
				startProjectContainerAdapter(t, ctx, client, containerOrchestrator, prepared)
				prepared = waitObjectAssumesStateEx(t, ctx, client, project.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return current.Status.DebugAdapterAddress != "", nil
				})
				host, port, addressErr := net.SplitHostPort(prepared.Status.DebugAdapterAddress)
				require.NoError(t, addressErr)
				require.Equal(t, "127.0.0.1", host)
				hostPort, portErr := strconv.Atoi(port)
				require.NoError(t, portErr)
				require.Positive(t, hostPort)
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, client.Get(ctx, types.NamespacedName{Name: prepared.Status.PhysicalContainerRef, Namespace: namespace.Name}, physical))
				listener := physical.Spec.Container.Ports[len(physical.Spec.Container.Ports)-1]
				require.Equal(t, expectedPort, listener.ContainerPort)
				require.Zero(t, listener.HostPort)
				require.Equal(t, "127.0.0.1", listener.HostIP)
				require.Equal(t, commonapi.PortProtocolTCP, listener.Protocol)
				require.Nil(t, prepared.Status.Debug, "container preparation must not launch an adapter")
			}
			require.NoError(t, client.Delete(ctx, project))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
		})
	}
}

func serviceProjectProcess(name, namespace, root string, arguments []string, environment []commonapi.EnvVar) *apiv2.ProjectProcess {
	resource := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: apiv2.ProjectProcessSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: root}, DebugAdapter: &apiv2.DebugAdapterConfig{Directory: root, Args: []string{"adapter"}}}}
	binding := apiv2.LogicalServiceBinding{Name: "http", Service: &apiv2.LogicalServiceTemplate{
		Name: "management", LogicalServiceSpec: apiv2.LogicalServiceSpec{Address: "127.0.0.1", Protocol: commonapi.PortProtocolUDP},
	}}

	resource.Spec.Process = apiv2.ProjectProcessTemplate{
		LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: namespace + "-command", Args: arguments, Env: environment,
			AmbientEnvironment: apiv2.AmbientEnvironment{Behavior: apiv2.EnvironmentBehaviorDoNotInherit}},
		Ports: []apiv2.LogicalProcessPort{{LogicalServiceBinding: binding, Address: "127.0.0.1"}},
	}

	return resource
}

func serviceProjectContainer(name, namespace, root string, arguments []string, environment []commonapi.EnvVar) *apiv2.ProjectContainer {
	resource := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: root}, DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: root, Args: []string{"adapter"}}}}}
	binding := apiv2.LogicalServiceBinding{Name: "http", Service: &apiv2.LogicalServiceTemplate{
		Name: "management", LogicalServiceSpec: apiv2.LogicalServiceSpec{Address: "127.0.0.1", Protocol: commonapi.PortProtocolUDP},
	}}

	resource.Spec.Container = apiv2.ProjectContainerTemplate{
		LogicalContainerConfig: apiv2.LogicalContainerConfig{
			Image: &apiv2.PhysicalContainerImageConfig{Image: namespace + "-image"}, Command: arguments, Env: environment,
		},
		Ports: []apiv2.LogicalContainerPort{{LogicalServiceBinding: binding, ContainerPort: 8080, Address: "127.0.0.1"}},
	}

	return resource
}

// Verifies that ProjectProcess prepares service-backed inputs without registering an application endpoint,
// preserves the declared values and keeps independently named services across replacement.
func TestV2ProjectProcessServicePortsAndEvaluatedValues(t *testing.T) {
	t.Parallel()
	kind := "process"

	t.Run(kind, func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		t.Cleanup(cancel)
		namespace := createV2ExecNamespace(t, ctx, client, "v2-project-services-"+kind)
		arguments := []string{`--listen={{addressForServing "http"}}:{{portForServing "http"}}`,
			`--consumer={{addressFor "project-management"}}:{{portFor "project-management"}}`}
		environment := []commonapi.EnvVar{
			{Name: "VALUE", Value: "{{"}, {Name: "VALUE", Value: `{{portForServing "http"}}`},
			{Name: "FRONT", Value: `{{addressFor "project-management"}}:{{portFor "project-management"}}`},
		}
		resource := serviceProjectProcess("project", namespace.Name, t.TempDir(), arguments, environment)
		require.NoError(t, client.Create(ctx, resource))
		running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
		require.Len(t, running.Status.Ports, 1)
		port := running.Status.Ports[0]
		require.Positive(t, port.Port)
		require.Equal(t, "project-management", port.ServiceRef)
		service := waitObjectAssumesStateEx(t, ctx, client, types.NamespacedName{Namespace: namespace.Name, Name: port.ServiceRef},
			func(current *apiv2.LogicalService) (bool, error) {
				return len(current.Status.EffectiveAddresses) != 0, nil
			})
		require.Empty(t, service.OwnerReferences)
		front := service.Status.EffectiveAddresses[0]
		expectedArgs := []string{fmt.Sprintf("--listen=127.0.0.1:%d", port.Port), fmt.Sprintf("--consumer=%s:%d", front.Address, front.Port)}
		expectedEnv := []commonapi.EnvVar{{Name: "VALUE", Value: fmt.Sprint(port.Port)}, {Name: "FRONT", Value: fmt.Sprintf("%s:%d", front.Address, front.Port)}}
		require.Equal(t, expectedArgs, running.Status.EffectiveArgs)
		require.ElementsMatch(t, expectedEnv, running.Status.EffectiveEnv)
		var endpoints apiv2.ServiceEndpointList
		require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
		require.Empty(t, endpoints.Items)

		var physicals apiv2.PhysicalProcessList
		require.NoError(t, client.List(ctx, &physicals, ctrl_client.InNamespace(namespace.Name)))
		require.Empty(t, physicals.Items)
		require.Equal(t, arguments, running.Spec.Process.Args)
		require.Equal(t, environment, running.Spec.Process.Env)

		require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
		waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
		require.NoError(t, client.Delete(ctx, resource))
		ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		retained := &apiv2.LogicalService{}
		require.NoError(t, client.Get(ctx, service.NamespacedName(), retained))
		require.Equal(t, service.UID, retained.UID)
		replacement := serviceProjectProcess("project", namespace.Name, t.TempDir(), arguments, environment)
		require.NoError(t, client.Create(ctx, replacement))
		replacementRunning := waitProjectPhase(t, ctx, client, replacement, apiv2.ProjectPhaseReady)
		require.NotEqual(t, resource.UID, replacement.UID)
		require.Equal(t, service.Name, replacementRunning.Status.Ports[0].ServiceRef)
		require.NoError(t, client.Get(ctx, service.NamespacedName(), retained))
		require.Equal(t, service.UID, retained.UID)
		require.NoError(t, client.Delete(ctx, replacement))
		ctrl_testutil.WaitObjectDeleted(t, ctx, client, replacement)
	})

}

// Verifies that ProjectContainer prepares service-backed inputs without registering an application endpoint,
// preserves the declared values and keeps independently named services across replacement.
func TestV2ProjectContainerServicePortsAndEvaluatedValues(t *testing.T) {
	t.Parallel()
	kind := "container"

	t.Run(kind, func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
		t.Cleanup(cancel)
		namespace := createV2ExecNamespace(t, ctx, client, "v2-project-services-"+kind)
		arguments := []string{`--listen={{addressForServing "http"}}:{{portForServing "http"}}`,
			`--consumer={{addressFor "project-management"}}:{{portFor "project-management"}}`}
		environment := []commonapi.EnvVar{
			{Name: "VALUE", Value: "{{"}, {Name: "VALUE", Value: `{{portForServing "http"}}`},
			{Name: "FRONT", Value: `{{addressFor "project-management"}}:{{portFor "project-management"}}`},
		}
		resource := serviceProjectContainer("project", namespace.Name, t.TempDir(), arguments, environment)
		require.NoError(t, client.Create(ctx, resource))
		running := waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
		require.Len(t, running.Status.Ports, 1)
		port := running.Status.Ports[0]
		require.Positive(t, port.Port)
		require.Equal(t, "project-management", port.ServiceRef)
		service := waitObjectAssumesStateEx(t, ctx, client, types.NamespacedName{Namespace: namespace.Name, Name: port.ServiceRef},
			func(current *apiv2.LogicalService) (bool, error) {
				return len(current.Status.EffectiveAddresses) != 0, nil
			})
		require.Empty(t, service.OwnerReferences)
		front := service.Status.EffectiveAddresses[0]
		expectedArgs := []string{fmt.Sprintf("--listen=127.0.0.1:%d", port.Port), fmt.Sprintf("--consumer=%s:%d", front.Address, front.Port)}
		expectedEnv := []commonapi.EnvVar{{Name: "VALUE", Value: fmt.Sprint(port.Port)}, {Name: "FRONT", Value: fmt.Sprintf("%s:%d", front.Address, front.Port)}}
		require.Equal(t, expectedArgs, running.Status.EffectiveArgs)
		require.ElementsMatch(t, expectedEnv, running.Status.EffectiveEnv)
		var endpoints apiv2.ServiceEndpointList
		require.NoError(t, client.List(ctx, &endpoints, ctrl_client.InNamespace(namespace.Name)))
		require.Empty(t, endpoints.Items)

		physical := &apiv2.PhysicalContainer{}
		require.NoError(t, client.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: running.Status.PhysicalContainerRef}, physical))
		require.Equal(t, "adapter", physical.Spec.Container.Entrypoint)
		require.Empty(t, physical.Spec.Container.Command)
		require.Equal(t, expectedEnv, physical.Spec.Container.Env)
		require.Equal(t, int32(8080), physical.Spec.Container.Ports[0].ContainerPort)
		require.Equal(t, commonapi.PortProtocolUDP, physical.Spec.Container.Ports[0].Protocol)
		require.Equal(t, arguments, running.Spec.Container.Command)
		require.Equal(t, environment, running.Spec.Container.Env)

		require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
		waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseStopped)
		require.NoError(t, client.Delete(ctx, resource))
		ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		retained := &apiv2.LogicalService{}
		require.NoError(t, client.Get(ctx, service.NamespacedName(), retained))
		require.Equal(t, service.UID, retained.UID)
		replacement := serviceProjectContainer("project", namespace.Name, t.TempDir(), arguments, environment)
		require.NoError(t, client.Create(ctx, replacement))
		replacementRunning := waitProjectPhase(t, ctx, client, replacement, apiv2.ProjectPhaseReady)
		require.NotEqual(t, resource.UID, replacement.UID)
		require.Equal(t, service.Name, replacementRunning.Status.Ports[0].ServiceRef)
		require.NoError(t, client.Get(ctx, service.NamespacedName(), retained))
		require.Equal(t, service.UID, retained.UID)
		require.NoError(t, client.Delete(ctx, replacement))
		ctrl_testutil.WaitObjectDeleted(t, ctx, client, replacement)
	})

}

// Verifies that a missing service template dependency retries without launching a backing,
// malformed templates remain terminal after Stop, and deletion can clean either preparation outcome.
func TestV2ProjectProcessTemplatePreparationFailures(t *testing.T) {
	t.Parallel()
	kind := "process"

	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("%s/malformed=%t", kind, malformed), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, fmt.Sprintf("v2-project-template-%s-%t", kind, malformed))
			arguments := []string{`{{portFor "absent"}}`}
			if malformed {
				arguments = []string{"{{"}
			}
			resource := serviceProjectProcess("project", namespace.Name, t.TempDir(), arguments, nil)

			resource.Spec.Process.Ports = nil

			require.NoError(t, client.Create(ctx, resource))
			expected := apiv2.ProjectPhasePending
			expectedReason := apiv2.ProjectReasonPreparing
			if malformed {
				expected = apiv2.ProjectPhaseFailed
				expectedReason = apiv2.ProjectReasonPreparationFailed
			}
			waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
				return current.Status.Phase == expected && len(current.Status.Conditions) != 0 &&
					current.Status.Conditions[0].Reason == string(expectedReason) &&
					current.Status.Conditions[0].Message != "", nil
			})
			if malformed {
				require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectProcess) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseFailed && current.Spec.Stop &&
						current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
				})
			} else {
				service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: namespace.Name},
					Spec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"}}
				require.NoError(t, client.Create(ctx, service))
				waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			}
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		})
	}

}

// Verifies that a missing service template dependency retries without launching a backing,
// malformed templates remain terminal after Stop, and deletion can clean either preparation outcome.
func TestV2ProjectContainerTemplatePreparationFailures(t *testing.T) {
	t.Parallel()
	kind := "container"

	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("%s/malformed=%t", kind, malformed), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, fmt.Sprintf("v2-project-template-%s-%t", kind, malformed))
			arguments := []string{`{{portFor "absent"}}`}
			if malformed {
				arguments = []string{"{{"}
			}
			resource := serviceProjectContainer("project", namespace.Name, t.TempDir(), arguments, nil)

			resource.Spec.Container.Ports = nil

			require.NoError(t, client.Create(ctx, resource))
			expected := apiv2.ProjectPhasePending
			expectedReason := apiv2.ProjectReasonPreparing
			if malformed {
				expected = apiv2.ProjectPhaseFailed
				expectedReason = apiv2.ProjectReasonPreparationFailed
			}
			waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
				return current.Status.Phase == expected && len(current.Status.Conditions) != 0 &&
					current.Status.Conditions[0].Reason == string(expectedReason) &&
					current.Status.Conditions[0].Message != "", nil
			})
			if malformed {
				require.NoError(t, client.Patch(ctx, resource, ctrl_client.RawPatch(types.MergePatchType, []byte(`{"spec":{"stop":true}}`))))
				waitObjectAssumesStateEx(t, ctx, client, resource.NamespacedName(), func(current *apiv2.ProjectContainer) (bool, error) {
					return current.Status.Phase == apiv2.ProjectPhaseFailed && current.Spec.Stop &&
						current.Status.Conditions[0].ObservedGeneration == current.Generation, nil
				})
			} else {
				service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "absent", Namespace: namespace.Name},
					Spec: apiv2.LogicalServiceSpec{Address: "127.0.0.1"}}
				require.NoError(t, client.Create(ctx, service))
				waitProjectPhase(t, ctx, client, resource, apiv2.ProjectPhaseReady)
			}
			require.NoError(t, client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, resource)
		})
	}

}

type projectEvaluatedCreateClient struct {
	ctrl_client.Client
	created chan ctrl_client.Object
	release chan struct{}
	lost    atomic.Bool
}

func (apiClient *projectEvaluatedCreateClient) Create(ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption) error {
	createErr := apiClient.Client.Create(ctx, object, options...)
	if createErr != nil {
		return createErr
	}
	owner := metav1.GetControllerOf(object)
	_, process := object.(*apiv2.PhysicalProcess)
	_, container := object.(*apiv2.PhysicalContainer)
	if owner != nil && owner.Kind == "ProjectContainer" && (process || container) && apiClient.lost.CompareAndSwap(false, true) {
		apiClient.created <- object.DeepCopyObject().(ctrl_client.Object)
		select {
		case <-apiClient.release:
		case <-ctx.Done():
		}
		return errors.New("evaluated backing create response lost")
	}
	return nil
}

// Verifies that ProjectContainer pins evaluated launch values after a committed physical create even if a managed
// service's producer addresses change before recovery, without evaluating or launching again.
func TestV2ProjectContainerPinsEvaluatedSnapshotAcrossLostCreate(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			var decorated *projectEvaluatedCreateClient
			server, _, startupErr := StartTestEnvironmentWithOptions(t, ctx, projectTestControllers|LogicalServiceController, t.Name(), t.TempDir(),
				TestEnvironmentOptions{DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
					decorated = &projectEvaluatedCreateClient{Client: apiClient, created: make(chan ctrl_client.Object, 1), release: make(chan struct{})}
					return decorated
				}})
			require.NoError(t, startupErr)
			release := sync.OnceFunc(func() { close(decorated.release) })
			t.Cleanup(release)
			namespace := createV2ExecNamespace(t, ctx, server.Client, "v2-project-snapshot-"+kind)
			service := &apiv2.LogicalService{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: namespace.Name},
				Spec: apiv2.LogicalServiceSpec{AddressAllocationMode: apiv2.ServiceAddressAllocationModeProxyless}}
			require.NoError(t, server.Client.Create(ctx, service))
			producer := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "producer", Namespace: namespace.Name},
				Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 43210}}}}
			require.NoError(t, server.Client.Create(ctx, producer))
			waitObjectAssumesStateEx(t, ctx, server.Client, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return current.Status.Phase == apiv2.LogicalServicePhaseRunning && len(current.Status.EffectiveAddresses) == 1 &&
					current.Status.EffectiveAddresses[0].Port == 43210, nil
			})
			resource := serviceProjectContainer("project", namespace.Name, t.TempDir(), []string{`{{portFor "source"}}`}, nil)

			resource.Spec.Container.Ports = nil

			require.NoError(t, server.Client.Create(ctx, resource))
			var backing ctrl_client.Object
			select {
			case backing = <-decorated.created:
			case <-ctx.Done():
				t.Fatal("Project did not declare evaluated execution")
			}
			require.NoError(t, server.Client.Delete(ctx, producer))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, producer)
			replacement := &apiv2.ServiceEndpoint{ObjectMeta: metav1.ObjectMeta{Name: producer.Name, Namespace: producer.Namespace},
				Spec: apiv2.ServiceEndpointSpec{ServiceRef: service.Name, Addresses: []apiv2.ServiceAddress{{Address: "127.0.0.1", Port: 54321}}}}
			require.NoError(t, server.Client.Create(ctx, replacement))
			require.NotEqual(t, producer.UID, replacement.UID)
			waitObjectAssumesStateEx(t, ctx, server.Client, service.NamespacedName(), func(current *apiv2.LogicalService) (bool, error) {
				return current.Status.Phase == apiv2.LogicalServicePhaseRunning && len(current.Status.EffectiveAddresses) == 1 &&
					current.Status.EffectiveAddresses[0].Port == 54321, nil
			})
			release()
			running := waitProjectPhase(t, ctx, server.Client, resource, apiv2.ProjectPhaseReady)
			require.Equal(t, backing.GetUID(), running.Status.RuntimeLogs.UID)

			physical := &apiv2.PhysicalContainer{}
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(backing), physical))
			require.Equal(t, []string{"43210"}, running.Status.EffectiveArgs)
			require.Equal(t, "adapter", physical.Spec.Container.Entrypoint)
			simulated := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.Equal(t, 1, simulated.CreateContainerCallCount(physical.Spec.Container.ContainerName))

			require.NoError(t, server.Client.Delete(ctx, resource))
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, resource)
		})
	}
}

// Verifies that ProjectContainer installs adapter files at the default or overridden destination before readiness,
// applies declared execute bits, reports missing sources/files as preparation failures, and does not launch an adapter.
func TestV2ProjectContainerAdapterDirectoryPreparation(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"default", "custom", "missing", "executable", "missing-executable"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			t.Cleanup(cancel)
			namespace := createV2ExecNamespace(t, ctx, client, "v2-project-adapter-directory-"+scenario)
			source := t.TempDir()
			require.NoError(t, usvc_io.WriteFile(filepath.Join(source, "dependency.dll"), []byte("dependency"), 0644))
			require.NoError(t, usvc_io.WriteFile(filepath.Join(source, "adapter"), []byte("binary"), 0644))
			project := &apiv2.ProjectContainer{ObjectMeta: metav1.ObjectMeta{Name: "project", Namespace: namespace.Name},
				Spec: apiv2.ProjectContainerSpec{ProjectCommonSpec: apiv2.ProjectCommonSpec{RootDirectory: t.TempDir()}, Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{
					Image: &apiv2.PhysicalContainerImageConfig{Image: namespace.Name + "-image"},
				}},
					DebugAdapter: &apiv2.ContainerDebugAdapterConfig{DebugAdapterConfig: apiv2.DebugAdapterConfig{Directory: source, Args: []string{"{{adapterDirectory}}/adapter"}}},
				}}
			destination := apiv2.DefaultContainerDebugAdapterDirectory
			if scenario == "custom" {
				destination = "/custom/adapter"
				project.Spec.DebugAdapter.ContainerDirectory = destination
			} else if scenario == "missing" {
				project.Spec.DebugAdapter.Directory = filepath.Join(source, "missing")
			} else if scenario == "executable" {
				project.Spec.DebugAdapter.ExecutableFiles = []string{"adapter"}
			} else if scenario == "missing-executable" {
				project.Spec.DebugAdapter.ExecutableFiles = []string{"missing"}
			}
			require.NoError(t, client.Create(ctx, project))
			expectedPhase := apiv2.ProjectPhaseReady
			if scenario == "missing" || scenario == "missing-executable" {
				expectedPhase = apiv2.ProjectPhaseFailed
			}
			observed := waitProjectPhase(t, ctx, client, project, expectedPhase)
			require.Nil(t, observed.Status.Debug)
			if scenario == "missing" || scenario == "missing-executable" {
				requireReadyCondition(t, observed.Status.Conditions, metav1.ConditionFalse, apiv2.ProjectReasonPreparationFailed)
				if scenario == "missing" {
					require.Contains(t, observed.Status.Conditions[0].Message, "directory source")
				} else {
					require.Contains(t, observed.Status.Conditions[0].Message, "executable file")
				}
			} else {
				physical := &apiv2.PhysicalContainer{}
				require.NoError(t, client.Get(ctx, ctrl_client.ObjectKey{Namespace: project.Namespace, Name: observed.Status.PhysicalContainerRef}, physical))
				require.Empty(t, physical.Spec.Container.CreateFiles)
				require.Len(t, physical.Spec.Container.CopyFiles, 1)
				require.Equal(t, source, physical.Spec.Container.CopyFiles[0].Directory)
				require.Equal(t, destination, physical.Spec.Container.CopyFiles[0].Destination)
				require.Equal(t, project.Spec.DebugAdapter.ExecutableFiles, physical.Spec.Container.CopyFiles[0].ExecutableFiles)
				require.Empty(t, physical.Spec.Container.CopyFiles[0].Source)
				require.False(t, physical.Spec.Container.CopyFiles[0].DeleteAfterCopy)
				require.DirExists(t, source)

				copied, copiedErr := containerOrchestrator.GetCreatedFiles(observed.Status.ContainerID)
				require.NoError(t, copiedErr)
				require.Len(t, copied, 1)
				reader := tar.NewReader(bytes.NewReader(copied[0].Tar.Bytes()))
				found := false
				foundExecutable := false
				for {
					header, nextErr := reader.Next()
					if nextErr == io.EOF {
						break
					}
					require.NoError(t, nextErr)
					if header.Name == strings.TrimPrefix(destination, "/")+"/adapter" && scenario == "executable" {
						require.EqualValues(t, 0111, header.Mode&0111)
						foundExecutable = true
					}
					if header.Name == strings.TrimPrefix(destination, "/")+"/dependency.dll" {
						payload, readErr := io.ReadAll(reader)
						require.NoError(t, readErr)
						require.Equal(t, "dependency", string(payload))
						found = true
					}
				}
				require.True(t, found)
				if scenario == "executable" {
					require.True(t, foundExecutable)
				}
			}
			require.NoError(t, client.Delete(ctx, project))
			ctrl_testutil.WaitObjectDeleted(t, ctx, client, project)
		})
	}
}
