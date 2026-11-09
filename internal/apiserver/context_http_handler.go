/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/microsoft/dcp/internal/contextdata"
	"github.com/microsoft/dcp/pkg/process"
)

const longRunningRequestShutdownWriteTimeout = time.Second

func withDcpContextValues(handler http.Handler, lifetimeCtx context.Context, log logr.Logger) http.Handler {
	return &dcpContextHandler{
		inner:           handler,
		lifetimeCtx:     lifetimeCtx,
		log:             log,
		processExecutor: process.NewOSExecutor(log),
	}
}

type dcpContextHandler struct {
	inner           http.Handler
	lifetimeCtx     context.Context
	log             logr.Logger
	processExecutor process.Executor
}

func (h *dcpContextHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), contextdata.HostLifetimeContextKey, h.lifetimeCtx)
	ctx = context.WithValue(ctx, contextdata.LoggerContextKey, h.log)
	ctx = context.WithValue(ctx, contextdata.ProcessExecutorContextKey, h.processExecutor)
	h.inner.ServeHTTP(w, r.WithContext(ctx))
}

func withLongRunningRequestCancellation(
	handler http.Handler,
	lifetimeCtx context.Context,
	longRunning apirequest.LongRunningRequestCheck,
	log logr.Logger,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestInfo, found := apirequest.RequestInfoFrom(r.Context())
		if !found {
			requestInfoErr := fmt.Errorf("request information is missing")
			log.Error(requestInfoErr, "Could not determine request lifetime")
			http.Error(w, requestInfoErr.Error(), http.StatusInternalServerError)
			return
		}

		if !longRunning(r, requestInfo) {
			handler.ServeHTTP(w, r)
			return
		}

		requestCtx, cancelRequest := context.WithCancel(r.Context())
		responseController := http.NewResponseController(w)
		writeDeadlineAppliedCh := make(chan bool, 1)
		stopLifetimeCancellation := context.AfterFunc(lifetimeCtx, func() {
			// Give cooperative context cancellation a bounded window to end the stream cleanly,
			// while ensuring a client-blocked response write cannot hold shutdown until the server timeout.
			writeDeadlineErr := responseController.SetWriteDeadline(time.Now().Add(longRunningRequestShutdownWriteTimeout))
			if writeDeadlineErr != nil {
				log.Error(writeDeadlineErr, "Could not bound long-running response write during API server shutdown")
			}
			writeDeadlineAppliedCh <- writeDeadlineErr == nil
			cancelRequest()
		})
		defer func() {
			if !stopLifetimeCancellation() {
				writeDeadlineApplied := <-writeDeadlineAppliedCh
				if writeDeadlineApplied {
					clearDeadlineErr := responseController.SetWriteDeadline(time.Time{})
					if clearDeadlineErr != nil {
						log.Error(clearDeadlineErr, "Could not clear long-running response write deadline")
					}
				}
			}
			cancelRequest()
		}()

		handler.ServeHTTP(w, r.WithContext(requestCtx))
	})
}
