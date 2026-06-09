/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package telemetry

import (
	"context"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// scopeAllowlistSpanProcessor forwards spans to its inner processor only when the
// span's instrumentation-scope name is in the configured allowlist.
// The OTLP-style startup profiler uses this to restrict export to the bounded set of startup-scoped
// spans; ordinary controller / runtime spans (which may carry application-defined
// resource names, container metadata, etc.) are dropped before they leave DCP, so
// enabling startup profiling never silently turns on a continuous export of
// application-runtime telemetry.
type scopeAllowlistSpanProcessor struct {
	allowed map[string]struct{}
	inner   sdktrace.SpanProcessor
}

func newScopeAllowlistSpanProcessor(inner sdktrace.SpanProcessor, allowedScopes ...string) sdktrace.SpanProcessor {
	set := make(map[string]struct{}, len(allowedScopes))
	for _, s := range allowedScopes {
		set[s] = struct{}{}
	}
	return &scopeAllowlistSpanProcessor{allowed: set, inner: inner}
}

// OnStart forwards unconditionally so the inner processor can manage its internal
// state; filtering is enforced in OnEnd which is where the export decision is made.
func (p *scopeAllowlistSpanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.inner.OnStart(ctx, s)
}

func (p *scopeAllowlistSpanProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if _, ok := p.allowed[s.InstrumentationScope().Name]; ok {
		p.inner.OnEnd(s)
	}
}

func (p *scopeAllowlistSpanProcessor) Shutdown(ctx context.Context) error {
	return p.inner.Shutdown(ctx)
}

func (p *scopeAllowlistSpanProcessor) ForceFlush(ctx context.Context) error {
	return p.inner.ForceFlush(ctx)
}
