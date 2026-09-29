/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"
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
	ctx           context.Context
	service       *apiv1.Service
	parentStorage rest.Storage
	statusStorage rest.Storage
	resourceWatch watch.Interface
	watchEvents   chan watch.Event
	fs            *mutationOrderingFS
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
		ctx:           namespacedCtx,
		service:       service,
		parentStorage: parentStorage,
		statusStorage: statusStorage,
		resourceWatch: resourceWatch,
		watchEvents:   watchEvents,
		fs:            fs,
	}
	t.Cleanup(func() {
		fs.releaseStatus()
		resourceWatch.Stop()
	})

	initialEvent := fixture.nextWatchEvent(t)
	require.Equal(t, watch.Added, initialEvent.Type)
	return fixture
}

func (f *mutationOrderingFixture) updateStatus() error {
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
	return updateErr
}

func (f *mutationOrderingFixture) delete() error {
	_, _, deleteErr := f.parentStorage.(rest.GracefulDeleter).Delete(
		f.ctx,
		f.service.Name,
		nil,
		&metav1.DeleteOptions{},
	)
	return deleteErr
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

func (f *mutationOrderingFixture) requireMutationResult(t *testing.T, result <-chan error, operation string) {
	t.Helper()
	select {
	case mutationErr := <-result:
		require.NoError(t, mutationErr)
	case <-f.ctx.Done():
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func (f *mutationOrderingFixture) requireStoredServiceDeleting(t *testing.T) {
	t.Helper()
	stored, getErr := f.parentStorage.(rest.Getter).Get(f.ctx, f.service.Name, &metav1.GetOptions{})
	require.NoError(t, getErr)
	require.NotNil(t, stored.(*apiv1.Service).DeletionTimestamp)
}

func TestTiltStoragePreservesWatchEventOrderDuringConcurrentMutations(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	fixture := newMutationOrderingFixture(t, ctx)

	statusResult := make(chan error, 1)
	go func() {
		statusResult <- fixture.updateStatus()
	}()
	select {
	case <-fixture.fs.statusWriteCommitted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for status write")
	}

	deletionStarted := make(chan struct{})
	deletionResult := make(chan error, 1)
	go func() {
		close(deletionStarted)
		deletionResult <- fixture.delete()
	}()
	select {
	case <-deletionStarted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for deletion to start")
	}
	select {
	case <-fixture.fs.deletionWriteCommitted:
		t.Fatal("deletion reached storage before the status event was published")
	default:
	}

	fixture.fs.releaseStatus()
	statusEvent := fixture.nextWatchEvent(t)
	deletionEvent := fixture.nextWatchEvent(t)
	fixture.requireMutationResult(t, statusResult, "status update")
	fixture.requireMutationResult(t, deletionResult, "deletion")
	fixture.requireStoredServiceDeleting(t)

	require.Nil(t, statusEvent.Object.(*apiv1.Service).DeletionTimestamp)
	require.NotNil(t, deletionEvent.Object.(*apiv1.Service).DeletionTimestamp)
	require.Less(
		t,
		resourceVersion(t, statusEvent),
		resourceVersion(t, deletionEvent),
		"Tilt did not preserve mutation commit order in the watch stream",
	)
}

func resourceVersion(t *testing.T, event watch.Event) uint64 {
	t.Helper()
	resource, validResource := event.Object.(tiltresource.Object)
	require.True(t, validResource)
	version, parseErr := strconv.ParseUint(resource.GetObjectMeta().ResourceVersion, 10, 64)
	require.NoError(t, parseErr)
	return version
}

type reconciliationStatusUpdate struct{}

func (reconciliationStatusUpdate) Preconditions() *metav1.Preconditions {
	return nil
}

func (reconciliationStatusUpdate) UpdatedObject(_ context.Context, oldObj runtime.Object) (runtime.Object, error) {
	updated := oldObj.(*apiv1.Service).DeepCopy()
	updated.Status.State = apiv1.ServiceStateReady
	return updated, nil
}

func BenchmarkTiltStorageMutationConcurrency(b *testing.B) {
	for _, objectCount := range []int{1, 32} {
		b.Run(fmt.Sprintf("%d-objects", objectCount), func(b *testing.B) {
			for _, mode := range []struct {
				name                  string
				serializeResourceKind bool
			}{
				{name: "native-per-object-lock"},
				{name: "coarse-resource-lock", serializeResourceKind: true},
			} {
				b.Run(mode.name, func(b *testing.B) {
					ctx, updater, names := newMutationBenchmarkStorage(b, objectCount)
					var resourceKindLock sync.Mutex
					updateErrors := make(chan error, objectCount)

					b.ResetTimer()
					for range b.N {
						var updates sync.WaitGroup
						updates.Add(len(names))
						for _, name := range names {
							go func() {
								defer updates.Done()
								if mode.serializeResourceKind {
									resourceKindLock.Lock()
									defer resourceKindLock.Unlock()
								}
								_, _, updateErr := updater.Update(
									ctx,
									name,
									reconciliationStatusUpdate{},
									nil,
									nil,
									false,
									&metav1.UpdateOptions{},
								)
								updateErrors <- updateErr
							}()
						}
						updates.Wait()
						for range names {
							if updateErr := <-updateErrors; updateErr != nil {
								b.Fatal(updateErr)
							}
						}
					}
					b.StopTimer()
					b.ReportMetric(
						float64(b.N*len(names))/b.Elapsed().Seconds(),
						"mutations/s",
					)
				})
			}
		})
	}
}

func newMutationBenchmarkStorage(
	b *testing.B,
	objectCount int,
) (context.Context, rest.Updater, []string) {
	b.Helper()

	scheme := runtime.NewScheme()
	if addSchemeErr := apiv1.AddToScheme(scheme); addSchemeErr != nil {
		b.Fatal(addSchemeErr)
	}
	metav1.AddToGroupVersion(scheme, apiv1.GroupVersion)
	codec := serializer.NewCodecFactory(scheme).LegacyCodec(apiv1.GroupVersion)
	serviceResource := &apiv1.Service{}
	storage := tiltfilepath.NewFilepathREST(
		tiltfilepath.NewMemoryFS(),
		tiltfilepath.NewWatchSet(),
		tiltrest.DefaultStrategy{
			Object:      serviceResource,
			ObjectTyper: scheme,
		},
		serviceResource.GetGroupVersionResource().GroupResource(),
		codec,
		"data",
		serviceResource.New,
		serviceResource.NewList,
	)
	ctx := genericapirequest.WithNamespace(context.Background(), mutationOrderingTestNamespace)
	names := make([]string, objectCount)
	for index := range objectCount {
		name := fmt.Sprintf("service-%d", index)
		names[index] = name
		_, createErr := storage.(rest.Creater).Create(
			ctx,
			&apiv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: mutationOrderingTestNamespace,
				},
			},
			nil,
			&metav1.CreateOptions{},
		)
		if createErr != nil {
			b.Fatal(createErr)
		}
	}
	return ctx, storage.(rest.Updater), names
}
