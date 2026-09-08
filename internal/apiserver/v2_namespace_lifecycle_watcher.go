/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	clientgorest "k8s.io/client-go/rest"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

const (
	v2NamespaceWatcherRestartInterval = 30 * time.Second
	v2NamespaceWatcherRetryInterval   = 2 * time.Second
)

type v2NamespaceWatchSource interface {
	List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

func newV2NamespaceWatchSource(
	clientConfig *clientgorest.Config,
) (v2NamespaceWatchSource, error) {
	dynamicClient, clientErr := dynamic.NewForConfig(clientConfig)
	if clientErr != nil {
		return nil, fmt.Errorf("create V2 Namespace watch client: %w", clientErr)
	}

	return dynamicClient.Resource((&apiv2.Namespace{}).GetGroupVersionResource()), nil
}

func runV2NamespaceLifecycleWatcher(
	ctx context.Context,
	source v2NamespaceWatchSource,
	gate *v2NamespaceLifecycleGate,
	log logr.Logger,
	restartInterval time.Duration,
	retryInterval time.Duration,
) {
	restartDelay := time.Duration(0)
	for {
		if !waitForV2NamespaceWatcherRestart(ctx, restartDelay) {
			return
		}

		closedStates := gate.closedNamespaceStates()
		namespaceList, listErr := source.List(ctx, metav1.ListOptions{})
		if listErr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error(listErr, "Failed to list V2 Namespaces while refreshing lifecycle state")
			restartDelay = retryInterval
			continue
		}
		gate.observeNamespaces(v2NamespaceNames(namespaceList), closedStates)

		namespaceWatcher, watchErr := source.Watch(ctx, metav1.ListOptions{})
		if watchErr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error(watchErr, "Failed to watch V2 Namespace deletion events")
			restartDelay = retryInterval
			continue
		}

		restartTimer := time.NewTimer(restartInterval)
		restartDelay = time.Duration(0)
		watcherClosed := false
	watchLoop:
		for {
			select {
			case <-ctx.Done():
				restartTimer.Stop()
				stopV2NamespaceWatcher(namespaceWatcher)
				return
			case <-restartTimer.C:
				log.V(1).Info("Restarting V2 Namespace lifecycle watcher")
				break watchLoop
			case event, open := <-namespaceWatcher.ResultChan():
				if !open {
					log.V(1).Info("V2 Namespace lifecycle watch stream closed")
					restartDelay = retryInterval
					watcherClosed = true
					break watchLoop
				}
				if event.Type == watch.Error {
					log.Info("V2 Namespace lifecycle watcher received an error event", "Object", event.Object)
					restartDelay = retryInterval
					break watchLoop
				}
				if event.Type != watch.Deleted {
					continue
				}
				log.V(1).Info("Observed V2 Namespace storage deletion")
				break watchLoop
			}
		}
		if !restartTimer.Stop() {
			select {
			case <-restartTimer.C:
			default:
			}
		}
		if !watcherClosed {
			stopV2NamespaceWatcher(namespaceWatcher)
		}
	}
}

func v2NamespaceNames(namespaceList *unstructured.UnstructuredList) map[string]struct{} {
	namespaces := make(map[string]struct{}, len(namespaceList.Items))
	for index := range namespaceList.Items {
		namespaces[namespaceList.Items[index].GetName()] = struct{}{}
	}
	return namespaces
}

func waitForV2NamespaceWatcherRestart(ctx context.Context, delay time.Duration) bool {
	if delay == 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func stopV2NamespaceWatcher(namespaceWatcher watch.Interface) {
	// Tilt broadcasts on an unbuffered channel while holding the WatchSet lock that Stop needs.
	// Keep draining until Stop closes the channel so an in-progress broadcast cannot deadlock.
	go drainV2NamespaceWatcher(namespaceWatcher.ResultChan())
	namespaceWatcher.Stop()
}

func drainV2NamespaceWatcher(events <-chan watch.Event) {
	for range events {
	}
}
