/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	deleteLease.complete(true)
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

type fakeV2NamespaceWatchSource struct {
	lock       sync.Mutex
	namespaces map[string]struct{}
	watchers   chan *watch.RaceFreeFakeWatcher
	beforeList func()
	afterList  func()
}

func newFakeV2NamespaceWatchSource(namespaces ...string) *fakeV2NamespaceWatchSource {
	source := &fakeV2NamespaceWatchSource{
		namespaces: map[string]struct{}{},
		watchers:   make(chan *watch.RaceFreeFakeWatcher, 10),
	}
	source.setNamespaces(namespaces...)
	return source
}

func (source *fakeV2NamespaceWatchSource) List(
	_ context.Context,
	_ metav1.ListOptions,
) (*unstructured.UnstructuredList, error) {
	if source.beforeList != nil {
		source.beforeList()
	}

	source.lock.Lock()
	namespaceList := &unstructured.UnstructuredList{}
	for namespace := range source.namespaces {
		namespaceList.Items = append(namespaceList.Items, *v2NamespaceObject(namespace))
	}
	source.lock.Unlock()

	if source.afterList != nil {
		source.afterList()
	}
	return namespaceList, nil
}

func (source *fakeV2NamespaceWatchSource) Watch(
	_ context.Context,
	_ metav1.ListOptions,
) (watch.Interface, error) {
	namespaceWatcher := watch.NewRaceFreeFake()
	source.watchers <- namespaceWatcher
	return namespaceWatcher, nil
}

func (source *fakeV2NamespaceWatchSource) setNamespaces(namespaces ...string) {
	source.lock.Lock()
	defer source.lock.Unlock()

	source.namespaces = make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		source.namespaces[namespace] = struct{}{}
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
	deleteLease.complete(true)
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
