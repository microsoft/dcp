/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"context"
	"net/http"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/internal/contextdata"
	"github.com/microsoft/dcp/pkg/process"
)

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
