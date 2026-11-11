/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/statestore"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

type undecodedBackingClient struct {
	ctrl_client.Client
	kind                string
	committedCreates    atomic.Uint32
	processVisibleLists atomic.Uint32
}

func (apiClient *undecodedBackingClient) Create(
	ctx context.Context, object ctrl_client.Object, options ...ctrl_client.CreateOption,
) error {
	if metav1.GetControllerOf(object) == nil || !apiClient.matchesBacking(object) {
		return apiClient.Client.Create(ctx, object, options...)
	}
	committed := object.DeepCopyObject().(ctrl_client.Object)
	createErr := apiClient.Client.Create(ctx, committed, options...)
	if createErr != nil {
		return createErr
	}
	apiClient.committedCreates.Add(1)
	return errors.New("physical backing Create committed without decoding its response")
}

func (apiClient *undecodedBackingClient) matchesBacking(object ctrl_client.Object) bool {
	switch object.(type) {
	case *apiv2.PhysicalContainer:
		return apiClient.kind == "container"
	case *apiv2.PhysicalContainerImage:
		return apiClient.kind == "image"
	case *apiv2.PhysicalContainerNetwork:
		return apiClient.kind == "network"
	case *apiv2.PhysicalProcess:
		return apiClient.kind == "process"
	case *apiv2.PhysicalContainerExec:
		return apiClient.kind == "exec"
	default:
		return false
	}
}

func (apiClient *undecodedBackingClient) List(
	ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) error {
	listErr := apiClient.Client.List(ctx, objects, options...)
	if listErr != nil {
		return listErr
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if listOptions.FieldSelector == nil {
		return nil
	}
	switch children := objects.(type) {
	case *apiv2.PhysicalContainerList:
		if apiClient.kind == "container" {
			children.Items = nil
		}
	case *apiv2.PhysicalContainerImageList:
		if apiClient.kind == "image" {
			children.Items = nil
		}
	case *apiv2.PhysicalContainerNetworkList:
		if apiClient.kind == "network" {
			children.Items = nil
		}
	case *apiv2.PhysicalProcessList:
		if apiClient.kind == "process" {
			children.Items = slices.DeleteFunc(children.Items, func(child apiv2.PhysicalProcess) bool {
				return child.Name != "visible"
			})
			if len(children.Items) != 0 {
				apiClient.processVisibleLists.Add(1)
			}
		}
	case *apiv2.PhysicalContainerExecList:
		if apiClient.kind == "exec" {
			children.Items = nil
		}
	}
	return nil
}

type undecodedBackingReader struct {
	ctrl_client.Reader
	kind             string
	logicalName      types.NamespacedName
	failDeletion     atomic.Bool
	deletionFailures atomic.Uint32
}

func (reader *undecodedBackingReader) Get(
	ctx context.Context, key ctrl_client.ObjectKey, object ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	getErr := reader.Reader.Get(ctx, key, object, options...)
	target := false
	var logical ctrl_client.Object = &apiv2.LogicalContainer{}
	switch object.(type) {
	case *apiv2.PhysicalContainer:
		target = reader.kind == "container"
	case *apiv2.PhysicalContainerImage:
		target = reader.kind == "image"
	case *apiv2.PhysicalContainerNetwork:
		target = reader.kind == "network"
		logical = &apiv2.LogicalContainerNetwork{}
	case *apiv2.PhysicalProcess:
		target = reader.kind == "process"
		logical = &apiv2.LogicalProcess{}
	case *apiv2.PhysicalContainerExec:
		target = reader.kind == "exec"
		logical = &apiv2.LogicalContainerExec{}
	}
	if !target || getErr != nil {
		return getErr
	}
	logicalErr := reader.Reader.Get(ctx, reader.logicalName, logical)
	if logicalErr != nil {
		return logicalErr
	}
	if logical.GetDeletionTimestamp().IsZero() && reader.kind == "process" {
		return errors.New("uncached Create confirmation unavailable")
	}
	if !logical.GetDeletionTimestamp().IsZero() && reader.failDeletion.Load() {
		reader.deletionFailures.Add(1)
		return errors.New("uncached deletion confirmation unavailable")
	}
	return nil
}

func (reader *undecodedBackingReader) List(
	ctx context.Context, objects ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) error {
	target := false
	switch objects.(type) {
	case *apiv2.PhysicalContainerList:
		target = reader.kind == "container"
	case *apiv2.PhysicalContainerImageList:
		target = reader.kind == "image"
	case *apiv2.PhysicalContainerNetworkList:
		target = reader.kind == "network"
	case *apiv2.PhysicalProcessList:
		target = reader.kind == "process"
	case *apiv2.PhysicalContainerExecList:
		target = reader.kind == "exec"
	}
	if target {
		return errors.New("unscoped uncached backing list")
	}
	return reader.Reader.List(ctx, objects, options...)
}

// Verifies that logical deletion confirms generated backing names after undecoded Create responses,
// retains the owner and lease while point reads fail, and also cleans up cached unexpected-name backings.
func TestV2LogicalDeletionConfirmsUnrecordedBackingsByName(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "image", "network", "process", "exec"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			logicalName := types.NamespacedName{Namespace: "v2-undecoded-backing-" + kind, Name: "logical"}
			var reader *undecodedBackingReader
			var backingClient *undecodedBackingClient
			included := LogicalContainerController | LogicalContainerNetworkController | PhysicalContainerController |
				PhysicalContainerImageController | PhysicalContainerNetworkController
			if kind == "process" {
				included = LogicalProcessController | PhysicalProcessController
			} else if kind == "exec" {
				included = containerExecTestControllers
			}
			server, environment, startErr := StartTestEnvironmentWithOptions(t, ctx,
				included,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						backingClient = &undecodedBackingClient{Client: apiClient, kind: kind}
						return backingClient
					},
					DecorateAPIReader: func(apiReader ctrl_client.Reader) ctrl_client.Reader {
						reader = &undecodedBackingReader{Reader: apiReader, kind: kind, logicalName: logicalName}
						reader.failDeletion.Store(true)
						return reader
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: logicalName.Namespace}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			var logical, unrelated ctrl_client.Object
			config := &apiv2.LogicalContainerConfig{
				Image: &apiv2.PhysicalContainerImageConfig{Image: logicalName.Namespace + "-image"},
			}
			if kind == "container" {
				image := &apiv2.PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{Name: "ready-image", Namespace: logicalName.Namespace},
					Spec:       apiv2.PhysicalContainerImageSpec{Image: config.Image},
				}
				require.NoError(t, server.Client.Create(ctx, image))
				waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
				})
				config = &apiv2.LogicalContainerConfig{ImageRef: image.Name}
			}
			if kind == "network" {
				logical = &apiv2.LogicalContainerNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: logicalName.Name, Namespace: logicalName.Namespace},
					Spec: apiv2.LogicalContainerNetworkSpec{
						NetworkName: logicalName.Namespace + "-runtime", Network: &apiv2.LogicalContainerNetworkConfig{},
					},
				}
				unrelated = &apiv2.PhysicalContainerNetwork{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: logicalName.Namespace},
					Spec:       apiv2.PhysicalContainerNetworkSpec{NetworkID: "unrelated-runtime"},
				}
			} else if kind == "process" {
				logical = &apiv2.LogicalProcess{
					ObjectMeta: metav1.ObjectMeta{Name: logicalName.Name, Namespace: logicalName.Namespace},
					Spec: apiv2.LogicalProcessSpec{Mode: apiv2.LogicalProcessModePersistent, ProcessName: logicalName.Namespace, Process: &apiv2.LogicalProcessConfig{
						ExecutablePath: "v2-undecoded-backing-process-command",
					}},
				}
				pid := int64(99999999)
				unrelated = &apiv2.PhysicalProcess{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: logicalName.Namespace},
					Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
				}
			} else if kind == "exec" {
				target := &apiv2.LogicalContainer{
					ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: logicalName.Namespace},
					Spec:       apiv2.LogicalContainerSpec{Container: config},
				}
				require.NoError(t, server.Client.Create(ctx, target))
				runningTarget := waitObjectAssumesStateEx(t, ctx, server.Client, target.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.LogicalContainerPhaseRunning, nil
				})
				logical = &apiv2.LogicalContainerExec{
					ObjectMeta: metav1.ObjectMeta{Name: logicalName.Name, Namespace: logicalName.Namespace},
					Spec: apiv2.LogicalContainerExecSpec{ContainerRef: target.Name,
						Execution: apiv2.ContainerExecConfig{Command: "unrecorded-exec"}},
				}
				unrelated = &apiv2.PhysicalContainerExec{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: logicalName.Namespace},
					Spec: apiv2.PhysicalContainerExecSpec{ContainerRef: runningTarget.Status.PhysicalContainerRef,
						Execution: apiv2.ContainerExecConfig{Command: "unrelated-exec"}},
				}
			} else {
				logical = &apiv2.LogicalContainer{
					ObjectMeta: metav1.ObjectMeta{Name: logicalName.Name, Namespace: logicalName.Namespace},
					Spec: apiv2.LogicalContainerSpec{
						ContainerName: logicalName.Namespace + "-runtime", Container: config,
					},
				}
				if kind == "image" {
					unrelated = &apiv2.PhysicalContainerImage{
						ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: logicalName.Namespace},
						Spec:       apiv2.PhysicalContainerImageSpec{ImageID: "unrelated-runtime"},
					}
				} else {
					unrelated = &apiv2.PhysicalContainer{
						ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: logicalName.Namespace},
						Spec:       apiv2.PhysicalContainerSpec{ContainerID: "unrelated-runtime"},
					}
				}
			}
			require.NoError(t, server.Client.Create(ctx, unrelated))
			require.NoError(t, server.Client.Create(ctx, logical))
			require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
				return backingClient.committedCreates.Load() > 0, nil
			}))
			backingName := types.NamespacedName{Namespace: logicalName.Namespace, Name: "logical-" + string(logical.GetUID())}
			var backing ctrl_client.Object
			switch kind {
			case "container":
				backing = waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalContainer) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerPhaseRunning, nil
				})
			case "image":
				backing = waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
				})
			case "network":
				backing = waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalContainerNetwork) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerNetworkPhaseReady, nil
				})
			case "process":
				backing = waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalProcess) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalProcessPhaseRunning, nil
				})
			case "exec":
				backing = waitObjectAssumesStateEx(t, ctx, server.Client, backingName, func(current *apiv2.PhysicalContainerExec) (bool, error) {
					return current.Status.Phase == apiv2.ContainerExecPhaseRunning, nil
				})
			}
			require.True(t, metav1.IsControlledBy(backing, logical))
			require.Contains(t, backing.GetFinalizers(), controllers.LogicalBackingFinalizer)
			var visible *apiv2.PhysicalProcess
			if kind == "process" {
				pid := int64(99999998)
				visible = &apiv2.PhysicalProcess{
					ObjectMeta: metav1.ObjectMeta{Name: "visible", Namespace: logicalName.Namespace, OwnerReferences: backing.GetOwnerReferences()},
					Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
				}
				controllers.InitializeLogicalBackingFinalizers(visible, apiv2.GroupName+"/physicalprocess-reconciler")
				require.NoError(t, server.Client.Create(ctx, visible))
				require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
					return backingClient.processVisibleLists.Load() > 0, nil
				}))
			}
			require.NoError(t, server.Client.Delete(ctx, logical))
			require.NoError(t, wait.PollUntilContextCancel(ctx, waitPollInterval, true, func(context.Context) (bool, error) {
				return reader.deletionFailures.Load() > 0, nil
			}))
			require.NoError(t, server.Client.Get(ctx, logicalName, logical))
			require.False(t, logical.GetDeletionTimestamp().IsZero())
			require.NotEmpty(t, logical.GetFinalizers())
			require.NoError(t, server.Client.Get(ctx, backingName, backing))
			require.True(t, backing.GetDeletionTimestamp().IsZero())
			if owner, processResource := logical.(*apiv2.LogicalProcess); processResource {
				require.NoError(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, owner, environment.ResourceLeaseOwner))
			}
			reader.failDeletion.Store(false)
			switch owner := logical.(type) {
			case *apiv2.LogicalContainer:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, owner)
			case *apiv2.LogicalContainerNetwork:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, owner)
			case *apiv2.LogicalContainerExec:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, owner)
			case *apiv2.LogicalProcess:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, owner)
				require.ErrorIs(t, environment.StateStore.VerifyResourceLeaseHeld(ctx, owner, environment.ResourceLeaseOwner), statestore.ErrResourceLeaseNotHeld)
				record, recordErr := environment.StateStore.GetPersistentProcess(ctx, owner.GetLeaseKey())
				require.NoError(t, recordErr)
				require.Equal(t, *backing.(*apiv2.PhysicalProcess).Status.PID, int64(record.PID))
			}
			switch child := backing.(type) {
			case *apiv2.PhysicalContainer:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				_, inspectErr := server.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{Containers: []string{child.Status.ContainerID}})
				require.ErrorIs(t, inspectErr, containers.ErrNotFound)
			case *apiv2.PhysicalContainerImage:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
			case *apiv2.PhysicalContainerNetwork:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				_, inspectErr := server.ContainerOrchestrator.InspectNetworks(ctx, containers.InspectNetworksOptions{Networks: []string{child.Status.NetworkID}})
				require.ErrorIs(t, inspectErr, containers.ErrNotFound)
			case *apiv2.PhysicalProcess:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, visible)
			case *apiv2.PhysicalContainerExec:
				ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
			}
			require.NoError(t, server.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(unrelated), unrelated))
			require.True(t, unrelated.GetDeletionTimestamp().IsZero())
		})
	}
}

type unobservedBackingClient struct {
	ctrl_client.Client
	images       bool
	created      chan ctrl_client.Object
	returnCreate <-chan struct{}
	observe      <-chan struct{}
}

func (visibility *unobservedBackingClient) Get(
	ctx context.Context, key ctrl_client.ObjectKey, obj ctrl_client.Object, options ...ctrl_client.GetOption,
) error {
	_, image := obj.(*apiv2.PhysicalContainerImage)
	_, physical := obj.(*apiv2.PhysicalContainer)
	if (visibility.images && image) || (!visibility.images && physical) {
		select {
		case <-visibility.observe:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return visibility.Client.Get(ctx, key, obj, options...)
}

func (visibility *unobservedBackingClient) Create(
	ctx context.Context, obj ctrl_client.Object, options ...ctrl_client.CreateOption,
) error {
	owner := metav1.GetControllerOf(obj)
	owned := owner != nil && owner.APIVersion == apiv2.GroupVersion.String() && owner.Kind == "LogicalContainer"
	if _, physical := obj.(*apiv2.PhysicalContainer); physical && owned && visibility.images {
		return fmt.Errorf("injected physical declaration failure")
	}
	createErr := visibility.Client.Create(ctx, obj, options...)
	if createErr != nil || !owned {
		return createErr
	}
	var created ctrl_client.Object
	switch child := obj.(type) {
	case *apiv2.PhysicalContainer:
		if !visibility.images {
			created = child.DeepCopy()
		}
	case *apiv2.PhysicalContainerImage:
		if visibility.images {
			created = child.DeepCopy()
		}
	}
	if created == nil {
		return nil
	}
	select {
	case visibility.created <- created:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-visibility.returnCreate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (visibility *unobservedBackingClient) List(
	ctx context.Context, list ctrl_client.ObjectList, options ...ctrl_client.ListOption,
) error {
	listErr := visibility.Client.List(ctx, list, options...)
	if listErr != nil {
		return listErr
	}
	select {
	case <-visibility.observe:
		return nil
	default:
	}
	listOptions := (&ctrl_client.ListOptions{}).ApplyOptions(options)
	if listOptions.FieldSelector == nil {
		return nil
	}
	if _, ownerQuery := listOptions.FieldSelector.RequiresExactMatch(".metadata.logicalContainerOwnerUID"); !ownerQuery {
		return nil
	}
	switch children := list.(type) {
	case *apiv2.PhysicalContainerList:
		if !visibility.images {
			children.Items = slices.DeleteFunc(children.Items, func(child apiv2.PhysicalContainer) bool { return child.Name != "duplicate" })
		}
	case *apiv2.PhysicalContainerImageList:
		if visibility.images {
			children.Items = slices.DeleteFunc(children.Items, func(child apiv2.PhysicalContainerImage) bool { return child.Name != "duplicate" })
		}
	}
	return nil
}

// Verifies that logical deletion finds unobserved creations by name alongside cached unexpected-name
// backings, waits for their cleanup, and leaves unrelated resources untouched.
func TestV2LogicalContainerDeletesCreatedBackingsBeforeCacheObservation(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		images bool
	}{
		{name: "duplicate-containers"},
		{name: "duplicate-images", images: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
			defer cancel()
			returnCreate, observe := make(chan struct{}), make(chan struct{})
			var returnOnce, observeOnce sync.Once
			allowCreateReturn := func() { returnOnce.Do(func() { close(returnCreate) }) }
			allowObservation := func() { observeOnce.Do(func() { close(observe) }) }
			defer allowCreateReturn()
			defer allowObservation()
			var visibility *unobservedBackingClient
			server, _, startErr := StartTestEnvironmentWithOptions(t, ctx,
				NamespaceController|LogicalContainerController|PhysicalContainerController|PhysicalContainerImageController,
				t.Name(), t.TempDir(), TestEnvironmentOptions{
					DecorateAPIClient: func(apiClient ctrl_client.Client) ctrl_client.Client {
						visibility = &unobservedBackingClient{
							Client: apiClient, images: testCase.images, created: make(chan ctrl_client.Object, 1),
							returnCreate: returnCreate, observe: observe,
						}
						return visibility
					},
				})
			require.NoError(t, startErr)
			namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "created-backing"}}
			require.NoError(t, server.Client.Create(ctx, namespace))
			waitObjectAssumesStateEx(t, ctx, server.Client, namespace.NamespacedName(), func(current *apiv2.Namespace) (bool, error) {
				return current.Status.Phase == apiv2.NamespacePhaseActive, nil
			})
			var unrelated ctrl_client.Object
			config := &apiv2.LogicalContainerConfig{ImageRef: "ready-image"}
			if testCase.images {
				config = &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "created-backing-image"}}
				unrelated = &apiv2.PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
					Spec:       apiv2.PhysicalContainerImageSpec{ImageID: "unrelated-image-id"},
				}
			} else {
				image := &apiv2.PhysicalContainerImage{
					ObjectMeta: metav1.ObjectMeta{Name: "ready-image", Namespace: namespace.Name},
					Spec:       apiv2.PhysicalContainerImageSpec{Image: &apiv2.PhysicalContainerImageConfig{Image: "ready-image"}},
				}
				require.NoError(t, server.Client.Create(ctx, image))
				waitObjectAssumesStateEx(t, ctx, server.Client, image.NamespacedName(), func(current *apiv2.PhysicalContainerImage) (bool, error) {
					return current.Status.Phase == apiv2.PhysicalContainerImagePhaseReady, nil
				})
				unrelated = &apiv2.PhysicalContainer{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace.Name},
					Spec:       apiv2.PhysicalContainerSpec{ContainerID: "unrelated-container-id"},
				}
			}
			require.NoError(t, server.Client.Create(ctx, unrelated))
			logical := &apiv2.LogicalContainer{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: namespace.Name},
				Spec:       apiv2.LogicalContainerSpec{ContainerName: "created-backing-runtime", Container: config},
			}
			require.NoError(t, server.Client.Create(ctx, logical))
			var created ctrl_client.Object
			select {
			case created = <-visibility.created:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var duplicate ctrl_client.Object
			switch child := created.(type) {
			case *apiv2.PhysicalContainer:
				duplicate = child.DeepCopy()
			case *apiv2.PhysicalContainerImage:
				duplicate = child.DeepCopy()
			}
			duplicate.SetName("duplicate")
			duplicate.SetUID("")
			duplicate.SetResourceVersion("")
			require.NoError(t, visibility.Client.Create(ctx, duplicate))
			backings := []ctrl_client.Object{created, duplicate}
			require.NoError(t, server.Client.Delete(ctx, logical))
			waitObjectAssumesStateEx(t, ctx, server.Client, logical.NamespacedName(), func(current *apiv2.LogicalContainer) (bool, error) {
				return !current.DeletionTimestamp.IsZero(), nil
			})
			allowCreateReturn()
			require.NoError(t, wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(readCtx context.Context) (bool, error) {
				for _, backing := range backings {
					getErr := visibility.Client.Get(readCtx, ctrl_client.ObjectKeyFromObject(backing), backing)
					if getErr != nil {
						return false, getErr
					}
					if backing.GetDeletionTimestamp().IsZero() {
						return false, nil
					}
				}
				return true, nil
			}))
			for _, backing := range backings {
				require.Contains(t, backing.GetFinalizers(), controllers.LogicalBackingFinalizer)
			}
			require.NoError(t, visibility.Client.Get(ctx, logical.NamespacedName(), logical))
			require.Contains(t, logical.Finalizers, apiv2.GroupName+"/logicalcontainer-reconciler")
			require.NoError(t, visibility.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(unrelated), unrelated))
			require.True(t, unrelated.GetDeletionTimestamp().IsZero())
			allowObservation()
			ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, logical)
			for _, backing := range backings {
				switch child := backing.(type) {
				case *apiv2.PhysicalContainer:
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				case *apiv2.PhysicalContainerImage:
					ctrl_testutil.WaitObjectDeleted(t, ctx, server.Client, child)
				}
			}
			require.NoError(t, visibility.Client.Get(ctx, ctrl_client.ObjectKeyFromObject(unrelated), unrelated))
			require.True(t, unrelated.GetDeletionTimestamp().IsZero())
			orchestrator := server.ContainerOrchestrator.(*ctrl_testutil.TestContainerOrchestrator)
			require.Zero(t, orchestrator.CreateContainerCallCount(logical.Spec.ContainerName))
			require.Zero(t, orchestrator.PullImageCallCount("created-backing-image"))
		})
	}
}
