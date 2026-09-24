/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/microsoft/dcp/pkg/testutil"
)

type signalingLocker struct {
	lockAttempted chan struct{}
	release       chan struct{}
	once          sync.Once
}

func newSignalingLocker() *signalingLocker {
	return &signalingLocker{
		lockAttempted: make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (l *signalingLocker) Lock() {
	l.once.Do(func() {
		close(l.lockAttempted)
	})
	<-l.release
}

func (l *signalingLocker) Unlock() {}

func TestOrderedResourceMutationHandlerSerializesStatusMutationWithResource(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()

	handled := make(chan struct{})
	handler := withOrderedResourceMutations(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(handled)
	})).(*orderedResourceMutationHandler)
	lock := newSignalingLocker()
	handler.locks.Store(resourceMutationKey{
		apiGroup: "usvc-dev.developer.microsoft.com",
		resource: "services",
	}, lock)

	request := newResourceRequest(ctx, "update", "usvc-dev.developer.microsoft.com", "services", "status")
	go handler.ServeHTTP(httptest.NewRecorder(), request)

	select {
	case <-lock.lockAttempted:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the resource mutation lock")
	}

	select {
	case <-handled:
		t.Fatal("status mutation reached storage before the resource mutation lock was released")
	default:
	}

	close(lock.release)
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the serialized mutation")
	}
}

func TestOrderedResourceMutationHandlerAllowsIndependentRequests(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		requestInfo *genericapirequest.RequestInfo
	}{
		"resource read": {
			requestInfo: &genericapirequest.RequestInfo{
				IsResourceRequest: true,
				Verb:              "get",
				APIGroup:          "usvc-dev.developer.microsoft.com",
				Resource:          "services",
			},
		},
		"different resource mutation": {
			requestInfo: &genericapirequest.RequestInfo{
				IsResourceRequest: true,
				Verb:              "update",
				APIGroup:          "usvc-dev.developer.microsoft.com",
				Resource:          "executables",
			},
		},
		"non-resource mutation": {
			requestInfo: &genericapirequest.RequestInfo{
				Verb: "patch",
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, time.Minute)
			defer cancel()

			handled := make(chan struct{})
			handler := withOrderedResourceMutations(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				close(handled)
			})).(*orderedResourceMutationHandler)
			lock := newSignalingLocker()
			handler.locks.Store(resourceMutationKey{
				apiGroup: "usvc-dev.developer.microsoft.com",
				resource: "services",
			}, lock)

			requestCtx := genericapirequest.WithRequestInfo(ctx, testCase.requestInfo)
			request := httptest.NewRequestWithContext(requestCtx, http.MethodGet, "/", nil)
			go handler.ServeHTTP(httptest.NewRecorder(), request)

			select {
			case <-handled:
			case <-lock.lockAttempted:
				close(lock.release)
				t.Fatal("independent request acquired the Service mutation lock")
			case <-ctx.Done():
				t.Fatal("timed out waiting for the independent request")
			}
		})
	}
}

func TestIsResourceMutation(t *testing.T) {
	t.Parallel()

	for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
		require.True(t, isResourceMutation(verb), "expected %q to be treated as a mutation", verb)
	}
	for _, verb := range []string{"get", "list", "watch", "connect", ""} {
		require.False(t, isResourceMutation(verb), "expected %q not to be treated as a mutation", verb)
	}
}

func newResourceRequest(
	ctx context.Context,
	verb string,
	apiGroup string,
	resource string,
	subresource string,
) *http.Request {
	requestInfo := &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		Verb:              verb,
		APIGroup:          apiGroup,
		Resource:          resource,
		Subresource:       subresource,
	}
	requestCtx := genericapirequest.WithRequestInfo(ctx, requestInfo)
	return httptest.NewRequestWithContext(requestCtx, http.MethodPatch, "/", nil)
}
