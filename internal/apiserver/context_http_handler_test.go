/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	kubeserverfilters "k8s.io/apiserver/pkg/server/filters"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that long-running request contexts are canceled when API server shutdown starts, including an already-canceled server lifetime.
func TestLongRunningRequestsCancelOnApiServerShutdown(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"/apis/usvc-dev.developer.microsoft.com/v1/executables?watch=true",
		"/apis/usvc-dev.developer.microsoft.com/v1/executables/test/" + apiv1.LogSubresourceName + "?follow=true",
		"/apis/usvc-dev.developer.microsoft.com/v2/logicalcontainers/test/" + apiv1.LogSubresourceName + "?follow=true",
	} {
		for _, alreadyCanceled := range []bool{false, true} {
			name := path
			if alreadyCanceled {
				name += "/already-canceled"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := testutil.GetTestContext(t, 0)
				defer cancel()
				lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
				defer cancelLifetime()
				if alreadyCanceled {
					cancelLifetime()
				}
				request := newRequestWithApiInfo(t, ctx, http.MethodGet, path)

				called := false
				handler := withLongRunningRequestCancellation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					require.NotSame(t, request.Context(), r.Context())
					cancelLifetime()
					select {
					case <-r.Context().Done():
						require.ErrorIs(t, r.Context().Err(), context.Canceled)
					case <-ctx.Done():
						t.Fatal("request did not observe API server shutdown")
					}
					w.WriteHeader(http.StatusNoContent)
				}), lifetimeCtx, kubeserverfilters.BasicLongRunningRequestCheck(
					sets.NewString("watch"),
					sets.NewString(apiv1.LogSubresourceName),
				), logr.Discard())

				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				require.True(t, called)
				require.Equal(t, http.StatusNoContent, response.Code)
				require.NoError(t, ctx.Err())
			})
		}
	}
}

// Verifies that finite requests, including the stop PATCH, keep their original context while API server shutdown begins.
func TestFiniteRequestsDrainDuringApiServerShutdown(t *testing.T) {
	t.Parallel()

	for _, requestData := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPatch, path: "/admin/execution"},
		{method: http.MethodGet, path: "/admin/execution"},
		{method: http.MethodGet, path: "/apis/usvc-dev.developer.microsoft.com/v1/executables"},
	} {
		t.Run(requestData.method+requestData.path, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
			defer cancelLifetime()
			request := newRequestWithApiInfo(t, ctx, requestData.method, requestData.path)

			called := false
			handler := withLongRunningRequestCancellation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				cancelLifetime()
				require.Same(t, request.Context(), r.Context())
				require.NoError(t, r.Context().Err())
				w.WriteHeader(http.StatusNoContent)
			}), lifetimeCtx, kubeserverfilters.BasicLongRunningRequestCheck(
				sets.NewString("watch"),
				sets.NewString(apiv1.LogSubresourceName),
			), logr.Discard())

			response := &deadlineTrackingResponseWriter{
				ResponseRecorder: httptest.NewRecorder(),
			}
			handler.ServeHTTP(response, request)
			require.True(t, called)
			require.Equal(t, http.StatusNoContent, response.Code)
			require.NoError(t, request.Context().Err())
			require.False(t, response.writeDeadlineSet.Load())
		})
	}
}

// Verifies that API server shutdown interrupts a blocked long-running response write and cancels its request context.
func TestLongRunningRequestShutdownInterruptsBlockedResponseWrite(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
	defer cancelLifetime()
	request := newRequestWithApiInfo(t, ctx, http.MethodGet, "/custom-stream")
	response := newBlockingDeadlineResponseWriter()
	t.Cleanup(response.releaseWrite)
	writeErrCh := make(chan error, 1)
	var servedCtx context.Context

	handler := withLongRunningRequestCancellation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedCtx = r.Context()
		_, writeErr := w.Write([]byte("stream data"))
		writeErrCh <- writeErr
	}), lifetimeCtx, func(*http.Request, *apirequest.RequestInfo) bool { return true }, logr.Discard())

	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handler.ServeHTTP(response, request)
	}()

	select {
	case <-response.writeStarted:
	case <-ctx.Done():
		t.Fatal("long-running response write did not start")
	}

	cancelLifetime()

	select {
	case writeErr := <-writeErrCh:
		require.ErrorIs(t, writeErr, os.ErrDeadlineExceeded)
	case <-ctx.Done():
		t.Fatal("blocked long-running response write was not interrupted")
	}

	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("long-running request handler did not return after shutdown")
	}

	require.NotNil(t, servedCtx)
	require.ErrorIs(t, servedCtx.Err(), context.Canceled)
	require.False(t, response.writeDeadline.IsZero())
	select {
	case <-response.writeDeadlineCleared:
	default:
		t.Fatal("long-running response write deadline was not cleared")
	}
}

// Verifies that long-running requests retain client cancellation and release their derived context when the handler returns.
func TestLongRunningRequestsReleaseTheirContext(t *testing.T) {
	t.Parallel()

	for _, cancelClient := range []bool{false, true} {
		name := "handler-completes"
		if cancelClient {
			name = "client-cancels"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
			defer cancelLifetime()
			clientCtx, cancelClientContext := context.WithCancel(ctx)
			defer cancelClientContext()

			var servedCtx context.Context
			handler := withLongRunningRequestCancellation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				servedCtx = r.Context()
				if cancelClient {
					cancelClientContext()
					require.ErrorIs(t, servedCtx.Err(), context.Canceled)
				} else {
					require.NoError(t, servedCtx.Err())
				}
				w.WriteHeader(http.StatusNoContent)
			}), lifetimeCtx, func(*http.Request, *apirequest.RequestInfo) bool { return true }, logr.Discard())

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newRequestWithApiInfo(t, clientCtx, http.MethodGet, "/custom-stream"))
			require.NotNil(t, servedCtx)
			require.ErrorIs(t, servedCtx.Err(), context.Canceled)
			require.NoError(t, lifetimeCtx.Err())
			require.Equal(t, http.StatusNoContent, response.Code)
		})
	}
}

// Verifies that missing Kubernetes request information fails explicitly instead of serving a request with an unknown lifetime.
func TestRequestLifetimeRejectsMissingRequestInfo(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	called := false
	handler := withLongRunningRequestCancellation(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), ctx, func(*http.Request, *apirequest.RequestInfo) bool { return true }, logr.Discard())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/execution", nil))

	require.False(t, called)
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Contains(t, response.Body.String(), "request information is missing")
}

type deadlineTrackingResponseWriter struct {
	*httptest.ResponseRecorder
	writeDeadlineSet atomic.Bool
}

func (w *deadlineTrackingResponseWriter) SetWriteDeadline(time.Time) error {
	w.writeDeadlineSet.Store(true)
	return nil
}

type blockingDeadlineResponseWriter struct {
	header               http.Header
	writeStarted         chan struct{}
	writeDeadlineSet     chan struct{}
	writeDeadlineCleared chan struct{}
	writeDeadline        time.Time
	releaseWrite         func()
}

func newBlockingDeadlineResponseWriter() *blockingDeadlineResponseWriter {
	response := &blockingDeadlineResponseWriter{
		header:               make(http.Header),
		writeStarted:         make(chan struct{}),
		writeDeadlineSet:     make(chan struct{}),
		writeDeadlineCleared: make(chan struct{}),
	}
	response.releaseWrite = sync.OnceFunc(func() {
		close(response.writeDeadlineSet)
	})
	return response
}

func (w *blockingDeadlineResponseWriter) Header() http.Header {
	return w.header
}

func (w *blockingDeadlineResponseWriter) Write([]byte) (int, error) {
	close(w.writeStarted)
	<-w.writeDeadlineSet
	return 0, os.ErrDeadlineExceeded
}

func (*blockingDeadlineResponseWriter) WriteHeader(int) {}

func (w *blockingDeadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		close(w.writeDeadlineCleared)
		return nil
	}
	w.writeDeadline = deadline
	w.releaseWrite()
	return nil
}

func newRequestWithApiInfo(t *testing.T, ctx context.Context, method, path string) *http.Request {
	t.Helper()
	request := httptest.NewRequestWithContext(ctx, method, path, nil)
	resolver := &apirequest.RequestInfoFactory{
		APIPrefixes:          sets.NewString("api", "apis"),
		GrouplessAPIPrefixes: sets.NewString("api"),
	}
	requestInfo, requestInfoErr := resolver.NewRequestInfo(request)
	require.NoError(t, requestInfoErr)
	return request.WithContext(apirequest.WithRequestInfo(request.Context(), requestInfo))
}
