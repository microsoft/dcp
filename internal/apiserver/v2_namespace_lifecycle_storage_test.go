/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	tiltapiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	tiltresource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	tiltrest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/rest"
	tiltfilepath "github.com/tilt-dev/tilt-apiserver/pkg/storage/filepath"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/endpoints/handlers/finisher"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2NamespaceLifecycleStorageHoldsChildLeaseAfterHTTPTimeout(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	writeStarted, releaseWrite := make(chan struct{}), make(chan struct{})
	fixture.fs.beforeWrite = func(obj runtime.Object) error {
		if _, child := obj.(*apiv2.PhysicalProcess); child {
			close(writeStarted)
			select {
			case <-releaseWrite:
			case <-fixture.ctx.Done():
				return fixture.ctx.Err()
			}
		}
		return nil
	}
	requestCtx, cancelRequest := context.WithCancel(fixture.ctx)
	defer cancelRequest()
	httpResult := make(chan error, 1)
	storageDone := make(chan struct{})
	go func() {
		_, finishErr := finisher.FinishRequest(requestCtx, func() (runtime.Object, error) {
			defer close(storageDone)
			return fixture.children.Create(requestCtx, newV2LifecycleChild(), nil, nil)
		})
		httpResult <- finishErr
	}()
	waitForSignal(t, fixture.ctx, writeStarted)
	cancelRequest()
	require.True(t, apierrors.IsTimeout(waitForError(t, fixture.ctx, httpResult)))

	deleteResult := make(chan error, 1)
	go func() {
		_, _, deleteErr := fixture.namespaces.Delete(fixture.ctx, "test", nil, nil)
		deleteResult <- deleteErr
	}()
	waitV2NamespaceGateClosed(t, fixture.ctx, fixture.gate, "test")
	select {
	case deleteErr := <-deleteResult:
		t.Fatalf("delete passed an unfinished storage create: %v", deleteErr)
	default:
	}
	close(releaseWrite)
	waitForSignal(t, fixture.ctx, storageDone)
	require.NoError(t, waitForError(t, fixture.ctx, deleteResult))
	_, childGetErr := fixture.children.Get(fixture.ctx, "child", nil)
	require.NoError(t, childGetErr)
	fixture.requireTerminating(t)
	requireNoV2NamespaceMutation(t, fixture.gate, "test")
}

func TestV2NamespaceLifecycleStorageHoldsDeleteLeaseAfterHTTPTimeout(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	namespaceWatch, watchErr := fixture.namespaces.Watch(fixture.ctx, nil)
	require.NoError(t, watchErr)
	stopWatch := sync.OnceFunc(func() { stopV2NamespaceWatcher(namespaceWatch) })
	defer stopWatch()
	persisted := make(chan struct{})
	fixture.fs.afterWrite = func(obj runtime.Object) {
		if namespace, namespaceOK := obj.(*apiv2.Namespace); namespaceOK && namespace.DeletionTimestamp != nil {
			close(persisted)
		}
	}
	requestCtx, cancelRequest := context.WithCancel(fixture.ctx)
	defer cancelRequest()
	httpResult := make(chan error, 1)
	storageDone := make(chan struct{})
	go func() {
		_, finishErr := finisher.FinishRequest(requestCtx, func() (runtime.Object, error) {
			defer close(storageDone)
			deleted, _, deleteErr := fixture.namespaces.Delete(requestCtx, "test", nil, nil)
			return deleted, deleteErr
		})
		httpResult <- finishErr
	}()
	waitForSignal(t, fixture.ctx, persisted)
	cancelRequest()
	require.True(t, apierrors.IsTimeout(waitForError(t, fixture.ctx, httpResult)))
	fixture.requireTerminating(t)
	waitV2NamespaceMutationReferences(t, fixture.ctx, fixture.gate, "test", 1)
	require.Empty(t, fixture.gate.closedNamespaceStates())
	_, createErr := fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
	require.True(t, apierrors.IsForbidden(createErr))

	stopWatch()
	waitForSignal(t, fixture.ctx, storageDone)
	requireNoV2NamespaceMutation(t, fixture.gate, "test")
	fixture.gate.lock.Lock()
	defer fixture.gate.lock.Unlock()
	state := fixture.gate.namespaces["test"]
	require.True(t, state.closed)
	require.True(t, state.deleteAccepted)
	require.Zero(t, state.activeDeletes)
}

func TestV2NamespaceLifecycleStorageSerializesNamespaceCreateAndDelete(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	writeStarted, releaseWrite := make(chan struct{}), make(chan struct{})
	fixture.fs.beforeWrite = func(obj runtime.Object) error {
		if namespace, namespaceOK := obj.(*apiv2.Namespace); namespaceOK && namespace.DeletionTimestamp == nil {
			close(writeStarted)
			select {
			case <-releaseWrite:
			case <-fixture.ctx.Done():
				return fixture.ctx.Err()
			}
		}
		return nil
	}
	createResult, deleteResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, createErr := fixture.namespaces.Create(fixture.ctx, newV2LifecycleNamespace(), nil, nil)
		createResult <- createErr
	}()
	waitForSignal(t, fixture.ctx, writeStarted)
	go func() {
		_, _, deleteErr := fixture.namespaces.Delete(fixture.ctx, "test", nil, nil)
		deleteResult <- deleteErr
	}()
	waitV2NamespaceMutationReferences(t, fixture.ctx, fixture.gate, "test", 2)
	select {
	case deleteErr := <-deleteResult:
		t.Fatalf("delete overtook namespace create: %v", deleteErr)
	default:
	}
	close(releaseWrite)
	require.NoError(t, waitForError(t, fixture.ctx, createResult))
	require.NoError(t, waitForError(t, fixture.ctx, deleteResult))
	fixture.requireTerminating(t)
}

func TestV2NamespaceLifecycleStorageSerializesNamespaceDeleteBeforeCreate(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	writeStarted, releaseWrite := make(chan struct{}), make(chan struct{})
	fixture.fs.beforeWrite = func(obj runtime.Object) error {
		if namespace, namespaceOK := obj.(*apiv2.Namespace); namespaceOK && namespace.DeletionTimestamp != nil {
			close(writeStarted)
			select {
			case <-releaseWrite:
			case <-fixture.ctx.Done():
				return fixture.ctx.Err()
			}
		}
		return nil
	}
	deleteResult, createResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, deleteErr := fixture.namespaces.Delete(fixture.ctx, "test", nil, nil)
		deleteResult <- deleteErr
	}()
	waitForSignal(t, fixture.ctx, writeStarted)
	go func() {
		_, createErr := fixture.namespaces.Create(fixture.ctx, newV2LifecycleNamespace(), nil, nil)
		createResult <- createErr
	}()
	waitV2NamespaceMutationReferences(t, fixture.ctx, fixture.gate, "test", 2)
	select {
	case createErr := <-createResult:
		t.Fatalf("create overtook namespace delete: %v", createErr)
	default:
	}
	close(releaseWrite)
	require.NoError(t, waitForError(t, fixture.ctx, deleteResult))
	require.True(t, apierrors.IsAlreadyExists(waitForError(t, fixture.ctx, createResult)))
	fixture.requireTerminating(t)
	requireV2NamespaceGateState(t, fixture.gate, "test")
}

func TestV2NamespaceLifecycleStorageReopensAfterTimedOutNamespaceCreateCompletes(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	deleteLease, closeErr := fixture.gate.beginDelete(fixture.ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(v2NamespaceMutationAccepted)
	writeStarted, releaseWrite := make(chan struct{}), make(chan struct{})
	fixture.fs.beforeWrite = func(runtime.Object) error {
		close(writeStarted)
		select {
		case <-releaseWrite:
			return nil
		case <-fixture.ctx.Done():
			return fixture.ctx.Err()
		}
	}
	requestCtx, cancelRequest := context.WithCancel(fixture.ctx)
	defer cancelRequest()
	httpResult := make(chan error, 1)
	storageDone := make(chan struct{})
	go func() {
		_, finishErr := finisher.FinishRequest(requestCtx, func() (runtime.Object, error) {
			defer close(storageDone)
			return fixture.namespaces.Create(requestCtx, newV2LifecycleNamespace(), nil, nil)
		})
		httpResult <- finishErr
	}()
	waitForSignal(t, fixture.ctx, writeStarted)
	cancelRequest()
	require.True(t, apierrors.IsTimeout(waitForError(t, fixture.ctx, httpResult)))
	requireV2NamespaceGateState(t, fixture.gate, "test")
	require.Empty(t, fixture.gate.closedNamespaceStates())
	close(releaseWrite)
	waitForSignal(t, fixture.ctx, storageDone)
	requireNoV2NamespaceGateState(t, fixture.gate, "test")
	requireNoV2NamespaceMutation(t, fixture.gate, "test")
	_, getErr := fixture.namespaces.Get(fixture.ctx, "test", nil)
	require.NoError(t, getErr)
}

func TestV2NamespaceLifecycleStorageReopensForReplacement(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	deleted, immediately, deleteErr := fixture.namespaces.Delete(fixture.ctx, "test", nil, nil)
	require.NoError(t, deleteErr)
	require.False(t, immediately)
	_, blockedErr := fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
	require.True(t, apierrors.IsForbidden(blockedErr))
	finalized := deleted.(*apiv2.Namespace).DeepCopy()
	finalized.Finalizers = nil
	_, _, finalizeErr := fixture.namespaces.Update(
		fixture.ctx, "test", rest.DefaultUpdatedObjectInfo(finalized), nil, nil, false, nil,
	)
	require.NoError(t, finalizeErr)
	requireV2NamespaceGateState(t, fixture.gate, "test")
	fixture.createNamespace(t)
	requireNoV2NamespaceGateState(t, fixture.gate, "test")
	_, childErr := fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
	require.NoError(t, childErr)
}

func TestV2NamespaceLifecycleStorageCancelledMutationDoesNotDispatch(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	firstLease, firstErr := fixture.gate.beginNamespaceMutation(fixture.ctx, "test")
	require.NoError(t, firstErr)
	defer firstLease.complete()
	requestCtx, cancelRequest := context.WithCancel(fixture.ctx)
	defer cancelRequest()
	deleteResult := make(chan error, 1)
	go func() {
		_, _, deleteErr := fixture.namespaces.Delete(requestCtx, "test", nil, nil)
		deleteResult <- deleteErr
	}()
	waitV2NamespaceMutationReferences(t, fixture.ctx, fixture.gate, "test", 2)
	cancelRequest()
	require.ErrorIs(t, waitForError(t, fixture.ctx, deleteResult), context.Canceled)
	namespace, getErr := fixture.namespaces.Get(fixture.ctx, "test", nil)
	require.NoError(t, getErr)
	require.Nil(t, namespace.(*apiv2.Namespace).DeletionTimestamp)
	requireNoV2NamespaceGateState(t, fixture.gate, "test")
}

func TestV2NamespaceLifecycleStorageReturnedErrorsAreRejected(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	_, _, deleteErr := fixture.namespaces.Delete(fixture.ctx, "missing", nil, nil)
	require.True(t, apierrors.IsNotFound(deleteErr))
	requireNoV2NamespaceGateState(t, fixture.gate, "missing")
	fixture.createNamespace(t)
	_, _, validationErr := fixture.namespaces.Delete(fixture.ctx, "test", func(context.Context, runtime.Object) error {
		return errors.New("rejected delete")
	}, nil)
	require.EqualError(t, validationErr, "rejected delete")
	requireNoV2NamespaceGateState(t, fixture.gate, "test")

	deleteLease, closeErr := fixture.gate.beginDelete(fixture.ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(v2NamespaceMutationAccepted)
	_, createErr := fixture.namespaces.Create(fixture.ctx, newV2LifecycleNamespace(), nil, nil)
	require.True(t, apierrors.IsAlreadyExists(createErr))
	requireV2NamespaceGateState(t, fixture.gate, "test")
	requireNoV2NamespaceMutation(t, fixture.gate, "test")
}

func TestV2NamespaceLifecycleStoragePanicsRequestAuthoritativeRefresh(t *testing.T) {
	for _, operation := range []string{"create", "delete"} {
		for _, afterWrite := range []bool{false, true} {
			stage := "before-write"
			if afterWrite {
				stage = "after-write"
			}
			t.Run(operation+"/"+stage, func(t *testing.T) {
				fixture := newV2NamespaceStorageFixture(t)
				if operation == "delete" {
					fixture.createNamespace(t)
				} else {
					deleteLease, closeErr := fixture.gate.beginDelete(fixture.ctx, "test")
					require.NoError(t, closeErr)
					deleteLease.complete(v2NamespaceMutationAccepted)
				}
				if afterWrite {
					fixture.fs.afterWrite = func(runtime.Object) { panic("storage panic") }
				} else {
					fixture.fs.beforeWrite = func(runtime.Object) error { panic("storage panic") }
				}
				require.PanicsWithValue(t, "storage panic", func() {
					if operation == "create" {
						_, _ = fixture.namespaces.Create(fixture.ctx, newV2LifecycleNamespace(), nil, nil)
					} else {
						_, _, _ = fixture.namespaces.Delete(fixture.ctx, "test", nil, nil)
					}
				})
				requireNoV2NamespaceMutation(t, fixture.gate, "test")
				waitForSignal(t, fixture.ctx, fixture.gate.refreshRequested)
				fixture.observeNamespaces(t)
				if operation == "delete" && afterWrite {
					fixture.requireTerminating(t)
					requireV2NamespaceGateState(t, fixture.gate, "test")
				} else {
					requireNoV2NamespaceGateState(t, fixture.gate, "test")
				}
			})
		}
	}
}

func TestV2NamespaceLifecycleStorageReleasesChildLeaseOnPanic(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.fs.beforeWrite = func(runtime.Object) error { panic("child panic") }
	require.PanicsWithValue(t, "child panic", func() {
		_, _ = fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
	})
	requireNoV2NamespaceGateState(t, fixture.gate, "test")
}

func TestV2NamespaceLifecycleStorageOnlyGatesCreateCapableChildUpdates(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	child, createErr := fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
	require.NoError(t, createErr)
	deleteLease, closeErr := fixture.gate.beginDelete(fixture.ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(v2NamespaceMutationAccepted)
	updated := child.(*apiv2.PhysicalProcess).DeepCopy()
	updated.Annotations = map[string]string{"test": "updated"}
	_, _, applyErr := fixture.children.Update(
		fixture.ctx, "child", rest.DefaultUpdatedObjectInfo(updated), nil, nil, true, nil,
	)
	require.True(t, apierrors.IsForbidden(applyErr))
	require.Contains(t, applyErr.Error(), "use update or a non-apply patch")
	_, _, updateErr := fixture.children.Update(
		fixture.ctx, "child", rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, nil,
	)
	require.NoError(t, updateErr)
}

func TestV2NamespaceLifecycleStorageUsesNamespaceUpdateCreationResult(t *testing.T) {
	for _, created := range []bool{false, true} {
		name := "updated"
		if created {
			name = "created"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newV2NamespaceStorageFixture(t)
			deleteLease, closeErr := fixture.gate.beginDelete(fixture.ctx, "test")
			require.NoError(t, closeErr)
			deleteLease.complete(v2NamespaceMutationAccepted)
			fixture.namespaces.StandardStorage = &v2LifecycleUpdateResultStorage{
				StandardStorage: fixture.namespaces.StandardStorage,
				created:         created,
			}
			_, _, updateErr := fixture.namespaces.Update(fixture.ctx, "test", nil, nil, nil, true, nil)
			require.NoError(t, updateErr)
			if created {
				requireNoV2NamespaceGateState(t, fixture.gate, "test")
			} else {
				requireV2NamespaceGateState(t, fixture.gate, "test")
			}
			requireNoV2NamespaceMutation(t, fixture.gate, "test")
		})
	}
}

func TestV2NamespaceLifecycleStorageFailedFinalizingUpdateIsUncertain(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	deleted, _, deleteErr := fixture.namespaces.Delete(fixture.ctx, "test", nil, nil)
	require.NoError(t, deleteErr)
	finalized := deleted.(*apiv2.Namespace).DeepCopy()
	finalized.Finalizers = nil
	fixture.fs.removeErr = errors.New("failed removal after write")
	_, _, updateErr := fixture.namespaces.Update(
		fixture.ctx, "test", rest.DefaultUpdatedObjectInfo(finalized), nil, nil, true, nil,
	)
	require.ErrorIs(t, updateErr, fixture.fs.removeErr)
	requireNoV2NamespaceMutation(t, fixture.gate, "test")
	fixture.gate.lock.Lock()
	state := fixture.gate.namespaces["test"]
	require.True(t, state.closed)
	require.Equal(t, v2NamespaceMutationCreate, state.uncertainMutation)
	fixture.gate.lock.Unlock()
	fixture.observeNamespaces(t)
	fixture.requireTerminating(t)
	requireV2NamespaceGateState(t, fixture.gate, "test")
}

func TestV2NamespaceLifecycleStorageRejectsDryRunWithoutMutations(t *testing.T) {
	for _, resource := range []string{"namespaces", "children"} {
		for _, operation := range []string{"create", "update", "apply", "delete", "deletecollection"} {
			t.Run(resource+"/"+operation, func(t *testing.T) {
				fixture := newV2NamespaceStorageFixture(t)
				fixture.createNamespace(t)
				_, childErr := fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
				require.NoError(t, childErr)
				storage := fixture.namespaces
				name := "test"
				if resource == "children" {
					storage, name = fixture.children, "child"
				}
				object, getErr := storage.Get(fixture.ctx, name, nil)
				require.NoError(t, getErr)
				before := object.DeepCopyObject()
				dryRun := []string{metav1.DryRunAll}
				var mutationErr error
				switch operation {
				case "create":
					_, mutationErr = storage.Create(fixture.ctx, object.DeepCopyObject(), nil, &metav1.CreateOptions{DryRun: dryRun})
				case "update", "apply":
					_, _, mutationErr = storage.Update(fixture.ctx, name, rest.DefaultUpdatedObjectInfo(object), nil, nil,
						operation == "apply", &metav1.UpdateOptions{DryRun: dryRun})
				case "delete":
					_, _, mutationErr = storage.Delete(fixture.ctx, name, nil, &metav1.DeleteOptions{DryRun: dryRun})
				case "deletecollection":
					_, mutationErr = storage.DeleteCollection(fixture.ctx, nil, &metav1.DeleteOptions{DryRun: dryRun}, nil)
				}
				if resource == "namespaces" && operation == "deletecollection" {
					require.True(t, apierrors.IsMethodNotSupported(mutationErr))
				} else {
					require.True(t, apierrors.IsBadRequest(mutationErr), "%v", mutationErr)
				}
				after, afterErr := storage.Get(fixture.ctx, name, nil)
				require.NoError(t, afterErr)
				require.Equal(t, before, after)
				requireNoV2NamespaceGateState(t, fixture.gate, "test")
				requireNoV2NamespaceMutation(t, fixture.gate, "test")
			})
		}
	}
}

func TestV2NamespaceLifecycleStorageDeleteCollectionScope(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	fixture.createNamespace(t)
	_, createErr := fixture.children.Create(fixture.ctx, newV2LifecycleChild(), nil, nil)
	require.NoError(t, createErr)
	_, namespaceErr := fixture.namespaces.DeleteCollection(fixture.ctx, nil, nil, nil)
	require.True(t, apierrors.IsMethodNotSupported(namespaceErr))
	_, childErr := fixture.children.DeleteCollection(fixture.ctx, nil, nil, nil)
	require.NoError(t, childErr)
	_, getErr := fixture.children.Get(fixture.ctx, "child", nil)
	require.True(t, apierrors.IsNotFound(getErr))
}

func TestV2NamespaceLifecycleStorageProviderPreservesInterfacesAndScope(t *testing.T) {
	fixture := newV2NamespaceStorageFixture(t)
	require.False(t, fixture.namespaces.NamespaceScoped())
	require.Contains(t, fixture.namespaces.ShortNames(), "ns")
	require.True(t, fixture.children.NamespaceScoped())
	namespace := fixture.createNamespace(t)
	table, tableErr := fixture.namespaces.ConvertToTable(fixture.ctx, namespace, &metav1.TableOptions{})
	require.NoError(t, tableErr)
	require.Len(t, table.Rows, 1)

	var providerCalls int
	provider := func(*runtime.Scheme, generic.RESTOptionsGetter) (rest.Storage, error) {
		providerCalls++
		return fixture.namespaces.StandardStorage, nil
	}
	config := &tiltapiserver.Config{ExtraConfig: tiltapiserver.ExtraConfig{
		APIs: map[schema.GroupVersionResource]tiltapiserver.StorageProvider{},
	}}
	for _, resource := range apiv2.PersistentTypes {
		config.ExtraConfig.APIs[resource.GetGroupVersionResource()] = provider
	}
	v1GVR := (&apiv1.Executable{}).GetGroupVersionResource()
	statusGVR := (&apiv2.Namespace{}).GetGroupVersionResource()
	statusGVR.Resource += "/status"
	config.ExtraConfig.APIs[v1GVR], config.ExtraConfig.APIs[statusGVR] = provider, provider
	require.NoError(t, decorateV2NamespaceStorageProviders(config, fixture.gate))
	require.Zero(t, providerCalls)
	for _, untouched := range []schema.GroupVersionResource{v1GVR, statusGVR} {
		unwrapped, providerErr := config.ExtraConfig.APIs[untouched](nil, nil)
		require.NoError(t, providerErr)
		require.Same(t, fixture.namespaces.StandardStorage, unwrapped)
	}
}

type v2NamespaceStorageFixture struct {
	ctx        context.Context
	gate       *v2NamespaceLifecycleGate
	fs         *v2LifecycleTestFS
	namespaces *v2NamespaceLifecycleStorage
	children   *v2NamespaceLifecycleStorage
}

func newV2NamespaceStorageFixture(t *testing.T) *v2NamespaceStorageFixture {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	t.Cleanup(cancel)
	gate := newV2NamespaceLifecycleGate()
	fs := &v2LifecycleTestFS{FS: tiltfilepath.NewMemoryFS()}
	fixture := &v2NamespaceStorageFixture{
		ctx:  requestinfo.WithNamespace(ctx, "test"),
		gate: gate,
		fs:   fs,
	}
	fixture.namespaces = newV2LifecycleTestStorage(t, &apiv2.Namespace{}, gate, fs)
	fixture.children = newV2LifecycleTestStorage(t, &apiv2.PhysicalProcess{}, gate, fs)
	return fixture
}

func newV2LifecycleTestStorage(
	t *testing.T,
	resource tiltresource.Object,
	gate *v2NamespaceLifecycleGate,
	fs tiltfilepath.FS,
) *v2NamespaceLifecycleStorage {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	metav1.AddToGroupVersion(scheme, apiv2.GroupVersion)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(apiv2.GroupVersion)
	provider := func(*runtime.Scheme, generic.RESTOptionsGetter) (rest.Storage, error) {
		return tiltfilepath.NewFilepathREST(
			fs, tiltfilepath.NewWatchSet(), tiltrest.DefaultStrategy{Object: resource, ObjectTyper: scheme},
			resource.GetGroupVersionResource().GroupResource(), codec, "data", resource.New, resource.NewList,
		), nil
	}
	storage, storageErr := withV2NamespaceLifecycleStorage(provider, resource.GetGroupVersionResource(), gate)(scheme, nil)
	require.NoError(t, storageErr)
	return storage.(*v2NamespaceLifecycleStorage)
}

func newV2LifecycleNamespace() *apiv2.Namespace {
	return &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
}

func newV2LifecycleChild() *apiv2.PhysicalProcess {
	pid := int64(2147483647)
	return &apiv2.PhysicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "test"},
		Spec:       apiv2.PhysicalProcessSpec{PID: &pid},
	}
}

func (fixture *v2NamespaceStorageFixture) createNamespace(t *testing.T) runtime.Object {
	t.Helper()
	namespace, createErr := fixture.namespaces.Create(fixture.ctx, newV2LifecycleNamespace(), nil, nil)
	require.NoError(t, createErr)
	return namespace
}

func (fixture *v2NamespaceStorageFixture) requireTerminating(t *testing.T) {
	t.Helper()
	namespace, getErr := fixture.namespaces.Get(fixture.ctx, "test", nil)
	require.NoError(t, getErr)
	require.NotNil(t, namespace.(*apiv2.Namespace).DeletionTimestamp)
}

func (fixture *v2NamespaceStorageFixture) observeNamespaces(t *testing.T) {
	t.Helper()
	list, listErr := fixture.namespaces.List(fixture.ctx, nil)
	require.NoError(t, listErr)
	namespaces := map[string]v2NamespaceStorageState{}
	for _, namespace := range list.(*apiv2.NamespaceList).Items {
		namespaces[namespace.Name] = v2NamespaceStorageState{terminating: namespace.DeletionTimestamp != nil}
	}
	fixture.gate.observeNamespaces(namespaces, fixture.gate.closedNamespaceStates())
}

type v2LifecycleTestFS struct {
	tiltfilepath.FS
	beforeWrite func(runtime.Object) error
	afterWrite  func(runtime.Object)
	removeErr   error
}

func (fs *v2LifecycleTestFS) Remove(path string) error {
	if fs.removeErr != nil {
		return fs.removeErr
	}
	return fs.FS.Remove(path)
}

func (fs *v2LifecycleTestFS) Write(encoder runtime.Encoder, path string, obj runtime.Object, version uint64) error {
	if fs.beforeWrite != nil {
		if beforeErr := fs.beforeWrite(obj); beforeErr != nil {
			return beforeErr
		}
	}
	if writeErr := fs.FS.Write(encoder, path, obj, version); writeErr != nil {
		return writeErr
	}
	if fs.afterWrite != nil {
		fs.afterWrite(obj)
	}
	return nil
}

type v2LifecycleUpdateResultStorage struct {
	rest.StandardStorage
	created bool
}

func (storage *v2LifecycleUpdateResultStorage) Update(
	context.Context, string, rest.UpdatedObjectInfo, rest.ValidateObjectFunc, rest.ValidateObjectUpdateFunc,
	bool, *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	return newV2LifecycleNamespace(), storage.created, nil
}
