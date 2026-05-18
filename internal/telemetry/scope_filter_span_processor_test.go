/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package telemetry

import (
	"context"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/microsoft/dcp/pkg/kubeconfig"
)

// captureProcessor records all spans it sees in OnEnd. Used by the scope-allowlist
// test below to verify that only allowed instrumentation scopes are forwarded.
type captureProcessor struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (p *captureProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (p *captureProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spans = append(p.spans, s)
}
func (p *captureProcessor) Shutdown(context.Context) error  { return nil }
func (p *captureProcessor) ForceFlush(context.Context) error { return nil }

// TestScopeAllowlistSpanProcessor_OnlyAllowedScopesAreForwarded is a security/privacy
// regression test: when Aspire startup profiling is enabled DCP attaches an OTLP
// exporter to its global TracerProvider. Without scope filtering this would silently
// turn on a continuous export of all DCP spans — including controller and runtime
// spans that may carry application-defined resource names. This test pins the
// allowlist behavior so any change that broadens the export surface fails loudly.
func TestScopeAllowlistSpanProcessor_OnlyAllowedScopesAreForwarded(t *testing.T) {
	capture := &captureProcessor{}
	filter := newScopeAllowlistSpanProcessor(capture, StartupTracerName, kubeconfig.TracerName)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(filter))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// Emit one span from each of: an allowed scope, the kubeconfig scope, and a
	// disallowed runtime scope. Only the first two should reach the inner processor.
	// The span name itself doesn't matter for the filter — only the tracer scope.
	_, s1 := tp.Tracer(StartupTracerName).Start(context.Background(), "dcp.startup")
	s1.End()
	_, s2 := tp.Tracer(kubeconfig.TracerName).Start(context.Background(), "dcp.kubeconfig.generate_certificate")
	s2.End()
	_, s3 := tp.Tracer("controller-common").Start(context.Background(), "saveChanges")
	s3.End()
	_, s4 := tp.Tracer("service-controller").Start(context.Background(), "reconcile")
	s4.End()

	if got, want := len(capture.spans), 2; got != want {
		t.Fatalf("forwarded span count = %d, want %d", got, want)
	}
	gotNames := map[string]bool{}
	for _, sp := range capture.spans {
		gotNames[sp.InstrumentationScope().Name] = true
	}
	if !gotNames[StartupTracerName] {
		t.Errorf("expected scope %q to be forwarded", StartupTracerName)
	}
	if !gotNames[kubeconfig.TracerName] {
		t.Errorf("expected scope %q to be forwarded", kubeconfig.TracerName)
	}
	for _, banned := range []string{"controller-common", "service-controller"} {
		if gotNames[banned] {
			t.Errorf("scope %q was forwarded but should have been filtered out", banned)
		}
	}
}

// TestScopeAllowlistSpanProcessor_PassesThroughLifecycle ensures Shutdown / ForceFlush
// delegate to the inner processor; these are not optional for span processors composed
// in a TracerProvider.
func TestScopeAllowlistSpanProcessor_PassesThroughLifecycle(t *testing.T) {
	inner := &lifecycleSpy{}
	filter := newScopeAllowlistSpanProcessor(inner, StartupTracerName)

	if err := filter.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if err := filter.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !inner.forceFlushed {
		t.Error("ForceFlush was not forwarded")
	}
	if !inner.shutdown {
		t.Error("Shutdown was not forwarded")
	}
}

type lifecycleSpy struct {
	forceFlushed bool
	shutdown     bool
}

func (s *lifecycleSpy) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (s *lifecycleSpy) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (s *lifecycleSpy) Shutdown(context.Context) error                  { s.shutdown = true; return nil }
func (s *lifecycleSpy) ForceFlush(context.Context) error                { s.forceFlushed = true; return nil }
