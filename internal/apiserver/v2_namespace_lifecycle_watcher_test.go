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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/microsoft/dcp/pkg/testutil"
)

func TestV2NamespaceLifecycleWatcherObservesDeletedEvent(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	source := newFakeV2NamespaceWatchSource("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	namespaceWatcher := waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	source.setNamespaces()
	namespaceWatcher.Delete(v2NamespaceObject("test"))

	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherRelistsAfterMissedEvent(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	source := newFakeV2NamespaceWatchSource("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), 10*time.Millisecond, time.Millisecond)

	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	source.setNamespaces()

	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherRelistsAfterClosedStream(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	source := newFakeV2NamespaceWatchSource("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	firstWatcher := waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	source.setNamespaces()
	firstWatcher.Stop()
	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)

	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherRelistsAfterErrorEvent(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	source := newFakeV2NamespaceWatchSource("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	firstWatcher := waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	source.setNamespaces()
	firstWatcher.Error(v2NamespaceObject("test"))
	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)

	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherImmediatelyRefreshesUncertainDelete(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	source := newFakeV2NamespaceWatchSource("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)

	deleteLease, deleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, deleteErr)
	deleteLease.complete(v2NamespaceMutationUncertain)

	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)
	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherKeepsUncertainTerminatingNamespaceClosed(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	source := newFakeV2NamespaceWatchSource()
	source.setTerminatingNamespaces("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)

	deleteLease, deleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, deleteErr)
	deleteLease.complete(v2NamespaceMutationUncertain)

	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)

	observeErr := wait.PollUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) (bool, error) {
		gate.lock.Lock()
		defer gate.lock.Unlock()
		current := gate.namespaces["test"]
		return current != nil && current.deleteAccepted && current.uncertainMutation == v2NamespaceMutationNone, nil
	})
	require.NoError(t, observeErr)
	gate.lock.Lock()
	state := gate.namespaces["test"]
	require.NotNil(t, state)
	require.True(t, state.closed)
	require.True(t, state.deleteAccepted)
	require.Equal(t, v2NamespaceMutationNone, state.uncertainMutation)
	gate.lock.Unlock()
}

func TestV2NamespaceLifecycleWatcherRefreshesAfterMutationLeaseCompletes(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()
	gate := newV2NamespaceLifecycleGate()
	source := newFakeV2NamespaceWatchSource("test")
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)
	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)

	mutationLease, mutationErr := gate.beginNamespaceMutation(ctx, "test")
	require.NoError(t, mutationErr)
	deleteLease, deleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, deleteErr)
	deleteLease.complete(v2NamespaceMutationUncertain)
	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)
	requireV2NamespaceGateState(t, gate, "test")

	mutationLease.complete()
	waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, source.lists)
	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherObservesDeletionStartedDuringList(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	source := newFakeV2NamespaceWatchSource("test")
	listStarted := make(chan struct{})
	continueList := make(chan struct{})
	var blockFirstList sync.Once
	source.beforeList = func() {
		blockFirstList.Do(func() {
			close(listStarted)
			<-continueList
		})
	}
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	namespaceWatcher := waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, listStarted)

	deleteLease, deleteErr := gate.beginDelete(ctx, "test")
	require.NoError(t, deleteErr)
	deleteLease.complete(v2NamespaceMutationAccepted)
	source.setNamespaces()
	go namespaceWatcher.Delete(v2NamespaceObject("test"))
	close(continueList)

	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

func TestV2NamespaceLifecycleWatcherObservesDeletionCompletedAfterList(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	source := newFakeV2NamespaceWatchSource("test")
	listCompleted := make(chan struct{})
	returnList := make(chan struct{})
	var blockFirstList sync.Once
	source.afterList = func() {
		blockFirstList.Do(func() {
			close(listCompleted)
			<-returnList
		})
	}
	go runV2NamespaceLifecycleWatcher(ctx, source, gate, logr.Discard(), time.Hour, time.Millisecond)

	namespaceWatcher := waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
	waitForV2NamespaceWatcherSignal(t, ctx, listCompleted)

	source.setNamespaces()
	go namespaceWatcher.Delete(v2NamespaceObject("test"))
	close(returnList)

	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

// Verifies that runV2NamespaceLifecycleWatcher drains unbuffered events while a list is blocked
// and refreshes lifecycle state after coalesced deletions.
func TestV2NamespaceLifecycleWatcherDrainsWhileListing(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	t.Cleanup(cancel)
	watcherCtx, cancelWatcher := context.WithCancel(ctx)

	gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
	source := newFakeV2NamespaceWatchSource("test")
	events := make(chan watch.Event)
	stopped := make(chan struct{})
	source.firstWatcher = watch.MockWatcher{
		StopFunc: sync.OnceFunc(func() {
			close(events)
			close(stopped)
		}),
		ResultChanFunc: func() <-chan watch.Event { return events },
	}
	listCompleted := make(chan struct{})
	returnList := make(chan struct{})
	releaseList := sync.OnceFunc(func() { close(returnList) })
	var blockFirstList sync.Once
	source.afterList = func() {
		blockFirstList.Do(func() {
			close(listCompleted)
			select {
			case <-returnList:
			case <-watcherCtx.Done():
			}
		})
	}

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		runV2NamespaceLifecycleWatcher(watcherCtx, source, gate, logr.Discard(), time.Hour, time.Millisecond)
	}()
	t.Cleanup(func() {
		cancelWatcher()
		waitForV2NamespaceWatcherSignal(t, ctx, watcherDone)
	})
	t.Cleanup(releaseList)
	waitForV2NamespaceWatcherSignal(t, ctx, listCompleted)

	source.setNamespaces()
	for _, eventType := range []watch.EventType{watch.Added, watch.Modified, watch.Deleted, watch.Deleted, watch.Modified} {
		sendV2NamespaceWatcherEvent(t, ctx, events, watch.Event{Type: eventType, Object: v2NamespaceObject("test")})
	}
	requireV2NamespaceGateState(t, gate, "test")
	releaseList()

	waitForV2NamespaceWatcherSignal(t, ctx, stopped)
	waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
}

// Verifies that runV2NamespaceLifecycleWatcher preserves watch errors and closure during a
// blocked list and continues draining after an error until the watch is stopped.
func TestV2NamespaceLifecycleWatcherRefreshesAfterWatchFailureDuringList(t *testing.T) {
	for _, failure := range []string{"error", "closed"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
			t.Cleanup(cancel)
			watcherCtx, cancelWatcher := context.WithCancel(ctx)
			gate := closedV2NamespaceLifecycleGate(t, ctx, "test")
			source := newFakeV2NamespaceWatchSource("test")
			events := make(chan watch.Event)
			stopEvents := sync.OnceFunc(func() { close(events) })
			source.firstWatcher = watch.MockWatcher{
				StopFunc:       stopEvents,
				ResultChanFunc: func() <-chan watch.Event { return events },
			}
			listCompleted := make(chan struct{})
			returnList := make(chan struct{})
			releaseList := sync.OnceFunc(func() { close(returnList) })
			var blockFirstList sync.Once
			source.afterList = func() {
				blockFirstList.Do(func() {
					close(listCompleted)
					select {
					case <-returnList:
					case <-watcherCtx.Done():
					}
				})
			}
			watcherDone := make(chan struct{})
			go func() {
				defer close(watcherDone)
				runV2NamespaceLifecycleWatcher(watcherCtx, source, gate, logr.Discard(), time.Hour, time.Millisecond)
			}()
			t.Cleanup(func() {
				cancelWatcher()
				waitForV2NamespaceWatcherSignal(t, ctx, watcherDone)
			})
			t.Cleanup(releaseList)
			waitForV2NamespaceWatcherSignal(t, ctx, listCompleted)

			source.setNamespaces()
			if failure == "error" {
				sendV2NamespaceWatcherEvent(t, ctx, events, watch.Event{
					Type: watch.Error, Object: &metav1.Status{Message: "test watch failure"},
				})
				sendV2NamespaceWatcherEvent(t, ctx, events, watch.Event{Type: watch.Modified, Object: v2NamespaceObject("test")})
			} else {
				stopEvents()
			}
			releaseList()

			waitForFakeV2NamespaceWatcher(t, ctx, source.watchers)
			waitForV2NamespaceGateRemoval(t, ctx, gate, "test")
		})
	}
}

// Verifies that runV2NamespaceLifecycleWatcher stops its watch and reader when the initial
// list fails or its lifetime context is canceled during the list.
func TestV2NamespaceLifecycleWatcherStopsWatchAfterListFailureOrCancellation(t *testing.T) {
	for _, failure := range []string{"list-error", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
			t.Cleanup(cancel)
			watcherCtx, cancelWatcher := context.WithCancel(ctx)
			source := newFakeV2NamespaceWatchSource("test")
			source.firstListErr = errors.New("test list failure")
			events := make(chan watch.Event)
			stopped := make(chan struct{})
			source.firstWatcher = watch.MockWatcher{
				StopFunc: sync.OnceFunc(func() {
					close(events)
					close(stopped)
				}),
				ResultChanFunc: func() <-chan watch.Event { return events },
			}
			listStarted := make(chan struct{})
			continueList := make(chan struct{})
			releaseList := sync.OnceFunc(func() { close(continueList) })
			var blockFirstList sync.Once
			source.beforeList = func() {
				blockFirstList.Do(func() {
					close(listStarted)
					select {
					case <-continueList:
					case <-watcherCtx.Done():
					}
				})
			}
			watcherDone := make(chan struct{})
			go func() {
				defer close(watcherDone)
				runV2NamespaceLifecycleWatcher(watcherCtx, source, newV2NamespaceLifecycleGate(), logr.Discard(), time.Hour, time.Millisecond)
			}()
			t.Cleanup(func() {
				cancelWatcher()
				waitForV2NamespaceWatcherSignal(t, ctx, watcherDone)
			})
			t.Cleanup(releaseList)
			waitForV2NamespaceWatcherSignal(t, ctx, listStarted)
			sendV2NamespaceWatcherEvent(t, ctx, events, watch.Event{Type: watch.Modified, Object: v2NamespaceObject("test")})

			if failure == "canceled" {
				cancelWatcher()
			} else {
				releaseList()
			}
			waitForV2NamespaceWatcherSignal(t, ctx, stopped)
			cancelWatcher()
			waitForV2NamespaceWatcherSignal(t, ctx, watcherDone)
		})
	}
}

// Verifies that runV2NamespaceLifecycleWatcher does not open a watch after cancellation.
func TestV2NamespaceLifecycleWatcherSkipsWatchAfterCancellation(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	cancel()
	source := newFakeV2NamespaceWatchSource()

	runV2NamespaceLifecycleWatcher(ctx, source, newV2NamespaceLifecycleGate(), logr.Discard(), time.Hour, time.Millisecond)

	select {
	case <-source.watchers:
		t.Fatal("opened a watch after cancellation")
	default:
	}
}

type fakeV2NamespaceWatchSource struct {
	lock         sync.Mutex
	namespaces   map[string]bool
	watchers     chan *watch.RaceFreeFakeWatcher
	lists        chan struct{}
	beforeList   func()
	afterList    func()
	firstWatcher watch.Interface
	firstListErr error
}

func newFakeV2NamespaceWatchSource(namespaces ...string) *fakeV2NamespaceWatchSource {
	source := &fakeV2NamespaceWatchSource{
		namespaces: map[string]bool{},
		watchers:   make(chan *watch.RaceFreeFakeWatcher, 10),
		lists:      make(chan struct{}, 10),
	}
	source.setNamespaces(namespaces...)
	return source
}

func (source *fakeV2NamespaceWatchSource) List(
	ctx context.Context,
	_ metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	if source.beforeList != nil {
		source.beforeList()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	source.lock.Lock()
	if source.firstListErr != nil {
		listErr := source.firstListErr
		source.firstListErr = nil
		source.lock.Unlock()
		return nil, listErr
	}
	namespaceList := &unstructured.UnstructuredList{}
	for namespace, terminating := range source.namespaces {
		namespaceObject := v2NamespaceObject(namespace)
		if terminating {
			deletionTimestamp := metav1.Now()
			namespaceObject.SetDeletionTimestamp(&deletionTimestamp)
		}
		namespaceList.Items = append(namespaceList.Items, *namespaceObject)
	}
	source.lock.Unlock()

	if source.afterList != nil {
		source.afterList()
	}
	source.lists <- struct{}{}
	return namespaceList, nil
}

func (source *fakeV2NamespaceWatchSource) Watch(
	_ context.Context,
	_ metav1.ListOptions,
) (watch.Interface, error) {
	source.lock.Lock()
	firstWatcher := source.firstWatcher
	source.firstWatcher = nil
	source.lock.Unlock()
	if firstWatcher != nil {
		return firstWatcher, nil
	}
	namespaceWatcher := watch.NewRaceFreeFake()
	source.watchers <- namespaceWatcher
	return namespaceWatcher, nil
}

func (source *fakeV2NamespaceWatchSource) setNamespaces(namespaces ...string) {
	source.lock.Lock()
	defer source.lock.Unlock()

	source.namespaces = make(map[string]bool, len(namespaces))
	for _, namespace := range namespaces {
		source.namespaces[namespace] = false
	}
}

func (source *fakeV2NamespaceWatchSource) setTerminatingNamespaces(namespaces ...string) {
	source.lock.Lock()
	defer source.lock.Unlock()

	source.namespaces = make(map[string]bool, len(namespaces))
	for _, namespace := range namespaces {
		source.namespaces[namespace] = true
	}
}

func closedV2NamespaceLifecycleGate(
	t *testing.T,
	ctx context.Context,
	namespace string,
) *v2NamespaceLifecycleGate {
	t.Helper()

	gate := newV2NamespaceLifecycleGate()
	deleteLease, deleteErr := gate.beginDelete(ctx, namespace)
	if deleteErr != nil {
		t.Fatal(deleteErr)
	}
	deleteLease.complete(v2NamespaceMutationAccepted)
	return gate
}

func v2NamespaceObject(namespace string) *unstructured.Unstructured {
	namespaceObject := &unstructured.Unstructured{}
	namespaceObject.SetName(namespace)
	return namespaceObject
}

func waitForFakeV2NamespaceWatcher(
	t *testing.T,
	ctx context.Context,
	watchers <-chan *watch.RaceFreeFakeWatcher,
) *watch.RaceFreeFakeWatcher {
	t.Helper()

	select {
	case namespaceWatcher := <-watchers:
		return namespaceWatcher
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func waitForV2NamespaceGateRemoval(
	t *testing.T,
	ctx context.Context,
	gate *v2NamespaceLifecycleGate,
	namespace string,
) {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		gate.lock.Lock()
		_, found := gate.namespaces[namespace]
		gate.lock.Unlock()
		if !found {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForV2NamespaceWatcherSignal(
	t *testing.T,
	ctx context.Context,
	signal <-chan struct{},
) {
	t.Helper()

	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func sendV2NamespaceWatcherEvent(t *testing.T, ctx context.Context, events chan<- watch.Event, event watch.Event) {
	t.Helper()

	select {
	case events <- event:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
