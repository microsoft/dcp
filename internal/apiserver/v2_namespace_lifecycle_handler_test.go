/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	requestinfo "k8s.io/apiserver/pkg/endpoints/request"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

const v2NamespaceLifecycleTestTimeout = 30 * time.Second

func TestV2NamespaceLifecycleGateWaitsForActiveCreates(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	release, allowed := gate.beginCreate("test")
	require.True(t, allowed)

	closeResult := make(chan error, 1)
	go func() {
		deleteLease, closeErr := gate.beginDelete(ctx, "test")
		if closeErr == nil {
			deleteLease.complete(true)
		}
		closeResult <- closeErr
	}()
	waitV2NamespaceGateClosed(t, ctx, gate, "test")

	_, allowed = gate.beginCreate("test")
	require.False(t, allowed)
	select {
	case closeErr := <-closeResult:
		require.Failf(t, "closeAndWait returned before create completed", "error: %v", closeErr)
	default:
	}

	release()
	select {
	case closeErr := <-closeResult:
		require.NoError(t, closeErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestV2NamespaceLifecycleHandlerSerializesCreateAndDelete(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	deleteStarted := make(chan struct{})
	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			close(createStarted)
			select {
			case <-releaseCreate:
			case <-request.Context().Done():
			}
			writer.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			close(deleteStarted)
			writer.WriteHeader(http.StatusAccepted)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	handler := newV2NamespaceLifecycleTestHandler(t, inner, gate)

	createRequest := newV2ResourceRequest(ctx, http.MethodPost, "test", "futurewidgets")
	createResponse := httptest.NewRecorder()
	createDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(createResponse, createRequest)
		close(createDone)
	}()
	select {
	case <-createStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	deleteRequest := httptest.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces/test",
		nil,
	)
	deleteResponse := httptest.NewRecorder()
	deleteDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(deleteResponse, deleteRequest)
		close(deleteDone)
	}()
	waitV2NamespaceGateClosed(t, ctx, gate, "test")

	select {
	case <-deleteStarted:
		require.Fail(t, "Namespace delete reached storage before resource create completed")
	default:
	}

	close(releaseCreate)
	select {
	case <-createDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-deleteDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	require.Equal(t, http.StatusCreated, createResponse.Code)
	require.Equal(t, http.StatusAccepted, deleteResponse.Code)

	blockedRequest := newV2ResourceRequest(ctx, http.MethodPost, "test", "anotherresource")
	blockedResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedResponse, blockedRequest)
	require.Equal(t, http.StatusForbidden, blockedResponse.Code)
}

func TestV2NamespaceLifecycleHandlerReopensGateForReplacementNamespace(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	var resourceCreateCalls atomic.Int32
	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/namespaces/test/") {
			resourceCreateCalls.Add(1)
		}
		writer.WriteHeader(http.StatusCreated)
	})
	gate := newV2NamespaceLifecycleGate()
	deleteLease, closeErr := gate.beginDelete(ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(true)
	handler := newV2NamespaceLifecycleTestHandler(t, inner, gate)

	blockedRequest := newV2ResourceRequest(ctx, http.MethodPost, "test", "physicalprocesses")
	blockedResponse := httptest.NewRecorder()
	handler.ServeHTTP(blockedResponse, blockedRequest)
	require.Equal(t, http.StatusForbidden, blockedResponse.Code)

	namespaceBody := `{"apiVersion":"` + apiv2.GroupVersion.String() + `","kind":"Namespace","metadata":{"name":"test"}}`
	namespaceRequest := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces",
		strings.NewReader(namespaceBody),
	)
	namespaceRequest.Header.Set("Content-Type", runtime.ContentTypeJSON)
	namespaceResponse := httptest.NewRecorder()
	handler.ServeHTTP(namespaceResponse, namespaceRequest)
	require.Equal(t, http.StatusCreated, namespaceResponse.Code)

	allowedRequest := newV2ResourceRequest(ctx, http.MethodPost, "test", "physicalprocesses")
	allowedResponse := httptest.NewRecorder()
	handler.ServeHTTP(allowedResponse, allowedRequest)
	require.Equal(t, http.StatusCreated, allowedResponse.Code)
	require.Equal(t, int32(1), resourceCreateCalls.Load())
}

func TestV2NamespaceLifecycleHandlerSerializesNamespaceCreateBeforeDelete(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	deleteStarted := make(chan struct{})
	handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			writer.WriteHeader(http.StatusCreated)
			close(createStarted)
			select {
			case <-releaseCreate:
			case <-request.Context().Done():
			}
		case http.MethodDelete:
			close(deleteStarted)
			writer.WriteHeader(http.StatusAccepted)
		}
	}), gate)

	createResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, newV2NamespaceCreateRequest(ctx, "create-first"))
		createResponse <- response
	}()
	waitForSignal(t, ctx, createStarted)

	deleteResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, newV2NamespaceDeleteRequest(ctx, "create-first"))
		deleteResponse <- response
	}()
	waitV2NamespaceMutationReferences(t, ctx, gate, "create-first", 2)
	select {
	case <-deleteStarted:
		t.Fatal("namespace delete reached storage before namespace create completed")
	default:
	}

	close(releaseCreate)
	require.Equal(t, http.StatusCreated, waitForResponse(t, ctx, createResponse).Code)
	require.Equal(t, http.StatusAccepted, waitForResponse(t, ctx, deleteResponse).Code)
	_, allowed := gate.beginCreate("create-first")
	require.False(t, allowed)
}

func TestV2NamespaceLifecycleHandlerSerializesNamespaceDeleteBeforeCreate(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	deleteStarted := make(chan struct{})
	releaseDelete := make(chan struct{})
	createStarted := make(chan struct{})
	handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodDelete:
			writer.WriteHeader(http.StatusAccepted)
			close(deleteStarted)
			select {
			case <-releaseDelete:
			case <-request.Context().Done():
			}
		case http.MethodPost:
			close(createStarted)
			writer.WriteHeader(http.StatusCreated)
		}
	}), gate)

	deleteResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, newV2NamespaceDeleteRequest(ctx, "delete-first"))
		deleteResponse <- response
	}()
	waitForSignal(t, ctx, deleteStarted)

	createResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, newV2NamespaceCreateRequest(ctx, "delete-first"))
		createResponse <- response
	}()
	waitV2NamespaceMutationReferences(t, ctx, gate, "delete-first", 2)
	select {
	case <-createStarted:
		t.Fatal("namespace create reached storage before namespace delete completed")
	default:
	}

	close(releaseDelete)
	require.Equal(t, http.StatusAccepted, waitForResponse(t, ctx, deleteResponse).Code)
	require.Equal(t, http.StatusCreated, waitForResponse(t, ctx, createResponse).Code)
	release, allowed := gate.beginCreate("delete-first")
	require.True(t, allowed)
	release()
}

func TestV2NamespaceLifecycleGateRemovesCancelledMutationWaiter(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	firstLease, firstLeaseErr := gate.beginNamespaceMutation(ctx, "test")
	require.NoError(t, firstLeaseErr)
	waitCtx, cancelWait := context.WithCancel(ctx)
	waitResult := make(chan error, 1)
	go func() {
		_, waitErr := gate.beginNamespaceMutation(waitCtx, "test")
		waitResult <- waitErr
	}()
	waitV2NamespaceMutationReferences(t, ctx, gate, "test", 2)

	cancelWait()
	require.ErrorIs(t, waitForError(t, ctx, waitResult), context.Canceled)
	firstLease.complete()
	requireNoV2NamespaceMutation(t, gate, "test")

	nextLease, nextLeaseErr := gate.beginNamespaceMutation(ctx, "test")
	require.NoError(t, nextLeaseErr)
	nextLease.complete()
	requireNoV2NamespaceMutation(t, gate, "test")
}

func TestV2NamespaceLifecycleHandlerReopensGateAfterFailedDelete(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		writer.WriteHeader(http.StatusCreated)
	})
	handler := newV2NamespaceLifecycleTestHandler(t, inner, newV2NamespaceLifecycleGate())

	deleteRequest := httptest.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces/test",
		nil,
	)
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	require.Equal(t, http.StatusConflict, deleteResponse.Code)

	createRequest := newV2ResourceRequest(ctx, http.MethodPost, "test", "futurewidgets")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	require.Equal(t, http.StatusCreated, createResponse.Code)
}

func TestV2NamespaceLifecycleHandlerCompletesDeleteLeaseOnPanic(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	}), gate)
	deleteRequest := httptest.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces/test",
		nil,
	)

	require.PanicsWithValue(t, "test panic", func() {
		handler.ServeHTTP(httptest.NewRecorder(), deleteRequest)
	})

	gate.lock.Lock()
	state := gate.namespaces["test"]
	require.NotNil(t, state)
	require.Zero(t, state.activeDeletes)
	require.True(t, state.closed)
	gate.lock.Unlock()
	requireNoV2NamespaceMutation(t, gate, "test")
}

func TestV2NamespaceLifecycleHandlerReopensGateWhenNamespaceCreatePanicsAfterSuccess(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	deleteLease, closeErr := gate.beginDelete(ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(true)
	handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		panic("test panic")
	}), gate)
	namespaceBody := `{"apiVersion":"` + apiv2.GroupVersion.String() + `","kind":"Namespace","metadata":{"name":"test"}}`
	namespaceRequest := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces",
		strings.NewReader(namespaceBody),
	)
	namespaceRequest.Header.Set("Content-Type", runtime.ContentTypeJSON)

	require.PanicsWithValue(t, "test panic", func() {
		handler.ServeHTTP(httptest.NewRecorder(), namespaceRequest)
	})

	release, allowed := gate.beginCreate("test")
	require.True(t, allowed)
	release()
	requireNoV2NamespaceMutation(t, gate, "test")
}

func TestV2NamespaceLifecycleHandlerKeepsGateClosedWhenConcurrentDeleteSucceeds(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	var deleteCalls atomic.Int32
	firstDeleteStarted := make(chan struct{})
	releaseFirstDelete := make(chan struct{})
	secondDeleteDone := make(chan struct{})
	inner := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete {
			writer.WriteHeader(http.StatusCreated)
			return
		}
		if deleteCalls.Add(1) == 1 {
			close(firstDeleteStarted)
			select {
			case <-releaseFirstDelete:
			case <-request.Context().Done():
			}
			writer.WriteHeader(http.StatusConflict)
			return
		}
		writer.WriteHeader(http.StatusAccepted)
		close(secondDeleteDone)
	})
	gate := newV2NamespaceLifecycleGate()
	handler := newV2NamespaceLifecycleTestHandler(t, inner, gate)
	deleteURL := "/apis/" + apiv2.GroupName + "/" + apiv2.Version + "/namespaces/test"

	firstDeleteDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil),
		)
		close(firstDeleteDone)
	}()
	select {
	case <-firstDeleteStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	secondDeleteResponse := httptest.NewRecorder()
	secondDeleteRequestDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(
			secondDeleteResponse,
			httptest.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil),
		)
		close(secondDeleteRequestDone)
	}()
	waitV2NamespaceMutationReferences(t, ctx, gate, "test", 2)
	select {
	case <-secondDeleteDone:
		t.Fatal("second namespace delete reached storage before the first delete completed")
	default:
	}

	close(releaseFirstDelete)
	select {
	case <-firstDeleteDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitForSignal(t, ctx, secondDeleteRequestDone)
	require.Equal(t, http.StatusAccepted, secondDeleteResponse.Code)

	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		createResponse,
		newV2ResourceRequest(ctx, http.MethodPost, "test", "futurewidgets"),
	)
	require.Equal(t, http.StatusForbidden, createResponse.Code)
}

func TestV2NamespaceLifecycleHandlerGatesServerSideApplyCreation(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	var innerCalls atomic.Int32
	gate := newV2NamespaceLifecycleGate()
	deleteLease, closeErr := gate.beginDelete(ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(true)
	handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		innerCalls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}), gate)
	request := httptest.NewRequestWithContext(
		ctx,
		http.MethodPatch,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces/test/futurewidgets/new",
		strings.NewReader(`{"apiVersion":"`+apiv2.GroupVersion.String()+`","kind":"FutureWidget","metadata":{"name":"new","namespace":"test"}}`),
	)
	request.Header.Set("Content-Type", string(types.ApplyPatchType))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusForbidden, response.Code)
	require.Zero(t, innerCalls.Load())
}

func TestV2NamespaceLifecycleHandlerLimitsNamespaceCreateBody(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	var innerCalls atomic.Int32
	handler := newV2NamespaceLifecycleTestHandlerWithMaxBody(
		t,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			innerCalls.Add(1)
		}),
		newV2NamespaceLifecycleGate(),
		32,
	)
	request := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces",
		strings.NewReader(`{"apiVersion":"`+apiv2.GroupVersion.String()+`","kind":"Namespace","metadata":{"name":"too-large"}}`),
	)
	request.Header.Set("Content-Type", runtime.ContentTypeJSON)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	require.Zero(t, innerCalls.Load())
}

func TestV2NamespaceLifecycleHandlerRejectsDeleteCollection(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	for _, requestPath := range []string{
		"/apis/" + apiv2.GroupName + "/" + apiv2.Version + "/namespaces",
		"/apis/" + apiv2.GroupName + "/" + apiv2.Version + "/namespaces/test/physicalprocesses",
	} {
		t.Run(requestPath, func(t *testing.T) {
			var innerCalls atomic.Int32
			handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				innerCalls.Add(1)
			}), newV2NamespaceLifecycleGate())
			request := httptest.NewRequestWithContext(ctx, http.MethodDelete, requestPath, nil)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			require.Equal(t, http.StatusMethodNotAllowed, response.Code)
			require.Zero(t, innerCalls.Load())
		})
	}
}

func TestV2NamespaceLifecycleHandlerDoesNotGateOtherAPIVersions(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, v2NamespaceLifecycleTestTimeout)
	defer cancel()

	gate := newV2NamespaceLifecycleGate()
	deleteLease, closeErr := gate.beginDelete(ctx, "test")
	require.NoError(t, closeErr)
	deleteLease.complete(true)
	handler := newV2NamespaceLifecycleTestHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	}), gate)
	request := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/apis/"+apiv2.GroupName+"/v1/namespaces/test/futurewidgets",
		nil,
	)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusCreated, response.Code)
}

func newV2NamespaceLifecycleTestHandler(
	t *testing.T,
	inner http.Handler,
	gate *v2NamespaceLifecycleGate,
) http.Handler {
	t.Helper()

	return newV2NamespaceLifecycleTestHandlerWithMaxBody(t, inner, gate, 1024*1024)
}

func newV2NamespaceLifecycleTestHandlerWithMaxBody(
	t *testing.T,
	inner http.Handler,
	gate *v2NamespaceLifecycleGate,
	maxBodyBytes int64,
) http.Handler {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	metav1.AddToGroupVersion(scheme, apiv2.GroupVersion)
	codecs := serializer.NewCodecFactory(scheme)
	resolver := &requestinfo.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
	return withV2NamespaceLifecycle(inner, gate, resolver, codecs, maxBodyBytes)
}

func newV2ResourceRequest(
	ctx context.Context,
	method string,
	namespace string,
	resource string,
) *http.Request {
	return httptest.NewRequestWithContext(
		ctx,
		method,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces/"+namespace+"/"+resource,
		nil,
	)
}

func newV2NamespaceCreateRequest(ctx context.Context, namespace string) *http.Request {
	body := fmt.Sprintf(`{"apiVersion":"%s","kind":"Namespace","metadata":{"name":%q}}`, apiv2.GroupVersion.String(), namespace)
	request := httptest.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", runtime.ContentTypeJSON)
	return request
}

func newV2NamespaceDeleteRequest(ctx context.Context, namespace string) *http.Request {
	return httptest.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		"/apis/"+apiv2.GroupName+"/"+apiv2.Version+"/namespaces/"+namespace,
		nil,
	)
}

func waitForSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()

	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitForResponse(
	t *testing.T,
	ctx context.Context,
	response <-chan *httptest.ResponseRecorder,
) *httptest.ResponseRecorder {
	t.Helper()

	select {
	case result := <-response:
		return result
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func waitForError(t *testing.T, ctx context.Context, result <-chan error) error {
	t.Helper()

	select {
	case resultErr := <-result:
		return resultErr
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil
	}
}

func requireNoV2NamespaceMutation(t *testing.T, gate *v2NamespaceLifecycleGate, namespace string) {
	t.Helper()

	gate.lock.Lock()
	defer gate.lock.Unlock()

	_, found := gate.namespaceMutations[namespace]
	require.False(t, found)
}

func waitV2NamespaceMutationReferences(
	t *testing.T,
	ctx context.Context,
	gate *v2NamespaceLifecycleGate,
	namespace string,
	references int,
) {
	t.Helper()

	for {
		gate.lock.Lock()
		state := gate.namespaceMutations[namespace]
		referenceCount := 0
		if state != nil {
			referenceCount = state.references
		}
		gate.lock.Unlock()
		if referenceCount == references {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			goruntime.Gosched()
		}
	}
}

func waitV2NamespaceGateClosed(
	t *testing.T,
	ctx context.Context,
	gate *v2NamespaceLifecycleGate,
	namespace string,
) {
	t.Helper()

	for {
		gate.lock.Lock()
		state := gate.namespaces[namespace]
		closed := state != nil && state.closed
		gate.lock.Unlock()
		if closed {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			goruntime.Gosched()
		}
	}
}
