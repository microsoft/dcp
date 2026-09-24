/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tiltresource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	tiltrest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/rest"
	tiltfilepath "github.com/tilt-dev/tilt-apiserver/pkg/storage/filepath"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/testutil"
)

const mutationOrderingTestNamespace = "test"

type mutationOrderingFS struct {
	tiltfilepath.FS

	blockStatusWrite       atomic.Bool
	statusWriteCommitted   chan struct{}
	deletionWriteCommitted chan struct{}
	releaseStatusWrite     chan struct{}
	statusCommitOnce       sync.Once
	deletionCommitOnce     sync.Once
	releaseOnce            sync.Once
}

func newMutationOrderingFS() *mutationOrderingFS {
	fs := &mutationOrderingFS{
		FS:                     tiltfilepath.NewMemoryFS(),
		statusWriteCommitted:   make(chan struct{}),
		deletionWriteCommitted: make(chan struct{}),
		releaseStatusWrite:     make(chan struct{}),
	}
	fs.blockStatusWrite.Store(true)
	return fs
}

func (fs *mutationOrderingFS) Write(
	encoder runtime.Encoder,
	path string,
	obj runtime.Object,
	storageVersion uint64,
) error {
	writeErr := fs.FS.Write(encoder, path, obj, storageVersion)
	if writeErr != nil {
		return writeErr
	}

	service, isService := obj.(*apiv1.Service)
	if !isService {
		return nil
	}
	if service.DeletionTimestamp != nil && !service.DeletionTimestamp.IsZero() {
		fs.deletionCommitOnce.Do(func() {
			close(fs.deletionWriteCommitted)
		})
		return nil
	}
	if service.Status.State == apiv1.ServiceStateReady && fs.blockStatusWrite.CompareAndSwap(true, false) {
		fs.statusCommitOnce.Do(func() {
			close(fs.statusWriteCommitted)
		})
		<-fs.releaseStatusWrite
	}
	return nil
}

func (fs *mutationOrderingFS) releaseStatus() {
	fs.releaseOnce.Do(func() {
		close(fs.releaseStatusWrite)
	})
}

type mutationOrderingFixture struct {
	ctx            context.Context
	service        *apiv1.Service
	parentStorage  rest.Storage
	statusStorage  rest.Storage
	resourceWatch  watch.Interface
	watchEvents    chan watch.Event
	fs             *mutationOrderingFS
	statusResult   chan error
	deletionResult chan error
}

func newMutationOrderingFixture(t *testing.T, ctx context.Context) *mutationOrderingFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	metav1.AddToGroupVersion(scheme, apiv1.GroupVersion)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(apiv1.GroupVersion)
	fs := newMutationOrderingFS()
	watchSet := tiltfilepath.NewWatchSet()
	serviceResource := &apiv1.Service{}
	defaultStrategy := tiltrest.DefaultStrategy{
		Object:      serviceResource,
		ObjectTyper: scheme,
	}
	parentStorage := tiltfilepath.NewFilepathREST(
		fs,
		watchSet,
		defaultStrategy,
		serviceResource.GetGroupVersionResource().GroupResource(),
		codec,
		"data",
		serviceResource.New,
		serviceResource.NewList,
	)
	statusStorage := tiltfilepath.NewFilepathREST(
		fs,
		watchSet,
		tiltrest.StatusSubResourceStrategy{Strategy: defaultStrategy},
		serviceResource.GetGroupVersionResource().GroupResource(),
		codec,
		"data",
		serviceResource.New,
		serviceResource.NewList,
	)
	namespacedCtx := genericapirequest.WithNamespace(ctx, mutationOrderingTestNamespace)
	service := &apiv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "service",
			Namespace:  mutationOrderingTestNamespace,
			Finalizers: []string{"test.dcp.microsoft.com/finalizer"},
		},
	}
	created, createErr := parentStorage.(rest.Creater).Create(
		namespacedCtx,
		service,
		nil,
		&metav1.CreateOptions{},
	)
	require.NoError(t, createErr)
	service = created.(*apiv1.Service)

	resourceWatch, watchErr := parentStorage.(rest.Watcher).Watch(
		namespacedCtx,
		&metainternalversion.ListOptions{},
	)
	require.NoError(t, watchErr)
	watchEvents := make(chan watch.Event, 3)
	go func() {
		for event := range resourceWatch.ResultChan() {
			watchEvents <- event
		}
	}()

	fixture := &mutationOrderingFixture{
		ctx:            namespacedCtx,
		service:        service,
		parentStorage:  parentStorage,
		statusStorage:  statusStorage,
		resourceWatch:  resourceWatch,
		watchEvents:    watchEvents,
		fs:             fs,
		statusResult:   make(chan error, 1),
		deletionResult: make(chan error, 1),
	}
	t.Cleanup(func() {
		fs.releaseStatus()
		resourceWatch.Stop()
	})

	initialEvent := fixture.nextWatchEvent(t)
	require.Equal(t, watch.Added, initialEvent.Type)
	return fixture
}

func (f *mutationOrderingFixture) handler() http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("test-operation") {
		case "status":
			updated := f.service.DeepCopy()
			updated.Status.State = apiv1.ServiceStateReady
			_, _, updateErr := f.statusStorage.(rest.Updater).Update(
				f.ctx,
				updated.Name,
				rest.DefaultUpdatedObjectInfo(updated),
				nil,
				nil,
				false,
				&metav1.UpdateOptions{},
			)
			f.statusResult <- updateErr
		case "delete":
			_, _, deleteErr := f.parentStorage.(rest.GracefulDeleter).Delete(
				f.ctx,
				f.service.Name,
				nil,
				&metav1.DeleteOptions{},
			)
			f.deletionResult <- deleteErr
		}
	})
}

func (f *mutationOrderingFixture) request(operation string, verb string, subresource string) *http.Request {
	requestInfo := &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		Verb:              verb,
		APIGroup:          apiv1.GroupVersion.Group,
		APIVersion:        apiv1.GroupVersion.Version,
		Namespace:         mutationOrderingTestNamespace,
		Resource:          f.service.GetGroupVersionResource().Resource,
		Subresource:       subresource,
		Name:              f.service.Name,
	}
	requestCtx := genericapirequest.WithRequestInfo(f.ctx, requestInfo)
	request := httptest.NewRequestWithContext(requestCtx, http.MethodPatch, "/", nil)
	request.Header.Set("test-operation", operation)
	return request
}

func (f *mutationOrderingFixture) nextWatchEvent(t *testing.T) watch.Event {
	t.Helper()
	select {
	case event := <-f.watchEvents:
		return event
	case <-f.ctx.Done():
		t.Fatal("timed out waiting for watch event")
		return watch.Event{}
	}
}

func (f *mutationOrderingFixture) requireMutationResults(t *testing.T) {
	t.Helper()
	select {
	case statusErr := <-f.statusResult:
		require.NoError(t, statusErr)
	case <-f.ctx.Done():
		t.Fatal("timed out waiting for status update")
	}
	select {
	case deletionErr := <-f.deletionResult:
		require.NoError(t, deletionErr)
	case <-f.ctx.Done():
		t.Fatal("timed out waiting for deletion")
	}
}

func (f *mutationOrderingFixture) requireStoredServiceDeleting(t *testing.T) {
	t.Helper()
	stored, getErr := f.parentStorage.(rest.Getter).Get(f.ctx, f.service.Name, &metav1.GetOptions{})
	require.NoError(t, getErr)
	require.NotNil(t, stored.(*apiv1.Service).DeletionTimestamp)
}

type observingMutex struct {
	sync.Mutex
	attempts      atomic.Int32
	secondAttempt chan struct{}
	secondOnce    sync.Once
}

func newObservingMutex() *observingMutex {
	return &observingMutex{secondAttempt: make(chan struct{})}
}

func (m *observingMutex) Lock() {
	if m.attempts.Add(1) == 2 {
		m.secondOnce.Do(func() {
			close(m.secondAttempt)
		})
	}
	m.Mutex.Lock()
}

func TestOrderedResourceMutationHandlerPreventsTiltWatchEventReordering(t *testing.T) {
	t.Parallel()

	t.Run("unserialized Tilt storage publishes stale event after deletion", func(t *testing.T) {
		ctx, cancel := testutil.GetTestContext(t, time.Minute)
		defer cancel()
		fixture := newMutationOrderingFixture(t, ctx)
		handler := fixture.handler()

		go handler.ServeHTTP(httptest.NewRecorder(), fixture.request("status", "update", "status"))
		select {
		case <-fixture.fs.statusWriteCommitted:
		case <-ctx.Done():
			t.Fatal("timed out waiting for status write")
		}

		go handler.ServeHTTP(httptest.NewRecorder(), fixture.request("delete", "delete", ""))
		select {
		case <-fixture.fs.deletionWriteCommitted:
		case <-ctx.Done():
			t.Fatal("timed out waiting for deletion write")
		}

		deletionEvent := fixture.nextWatchEvent(t)
		fixture.fs.releaseStatus()
		staleStatusEvent := fixture.nextWatchEvent(t)
		fixture.requireMutationResults(t)
		fixture.requireStoredServiceDeleting(t)

		require.NotNil(t, deletionEvent.Object.(*apiv1.Service).DeletionTimestamp)
		require.Nil(t, staleStatusEvent.Object.(*apiv1.Service).DeletionTimestamp)
		require.Greater(
			t,
			resourceVersion(t, deletionEvent),
			resourceVersion(t, staleStatusEvent),
			"Tilt published an older non-deleting object after the deletion event",
		)
	})

	t.Run("middleware preserves storage and watch event order", func(t *testing.T) {
		ctx, cancel := testutil.GetTestContext(t, time.Minute)
		defer cancel()
		fixture := newMutationOrderingFixture(t, ctx)
		handler := withOrderedResourceMutations(fixture.handler()).(*orderedResourceMutationHandler)
		mutationLock := newObservingMutex()
		handler.locks.Store(resourceMutationKey{
			apiGroup: apiv1.GroupVersion.Group,
			resource: fixture.service.GetGroupVersionResource().Resource,
		}, mutationLock)

		go handler.ServeHTTP(httptest.NewRecorder(), fixture.request("status", "update", "status"))
		select {
		case <-fixture.fs.statusWriteCommitted:
		case <-ctx.Done():
			t.Fatal("timed out waiting for status write")
		}

		go handler.ServeHTTP(httptest.NewRecorder(), fixture.request("delete", "delete", ""))
		select {
		case <-mutationLock.secondAttempt:
		case <-ctx.Done():
			t.Fatal("timed out waiting for deletion to contend on the mutation lock")
		}
		select {
		case <-fixture.fs.deletionWriteCommitted:
			t.Fatal("deletion reached storage before the status event was published")
		default:
		}

		fixture.fs.releaseStatus()
		statusEvent := fixture.nextWatchEvent(t)
		deletionEvent := fixture.nextWatchEvent(t)
		fixture.requireMutationResults(t)
		fixture.requireStoredServiceDeleting(t)

		require.Nil(t, statusEvent.Object.(*apiv1.Service).DeletionTimestamp)
		require.NotNil(t, deletionEvent.Object.(*apiv1.Service).DeletionTimestamp)
		require.Less(
			t,
			resourceVersion(t, statusEvent),
			resourceVersion(t, deletionEvent),
			"middleware did not preserve mutation commit order in the watch stream",
		)
	})
}

func resourceVersion(t *testing.T, event watch.Event) uint64 {
	t.Helper()
	resource, validResource := event.Object.(tiltresource.Object)
	require.True(t, validResource)
	version, parseErr := strconv.ParseUint(resource.GetObjectMeta().ResourceVersion, 10, 64)
	require.NoError(t, parseErr)
	return version
}
