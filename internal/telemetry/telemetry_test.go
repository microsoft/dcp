/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func TestTelemetryResourceSetsServiceName(t *testing.T) {
	resource := newTelemetryResource("dcp")

	value, found := resource.Set().Value(attribute.Key("service.name"))
	if !found {
		t.Fatal("expected service.name resource attribute")
	}

	if value.AsString() != "dcp" {
		t.Fatalf("unexpected service.name: %q", value.AsString())
	}
}

func TestStartupContextFromAnnotationsExtractsRemoteSpanContext(t *testing.T) {
	startupContext, ok := StartupContextFromAnnotations(map[string]string{
		StartupOperationIDAnnotation: "operation-id",
		StartupTraceParentAnnotation: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		StartupTraceStateAnnotation:  "vendor=value",
	})
	if !ok {
		t.Fatal("expected startup context")
	}

	if startupContext.OperationID != "operation-id" {
		t.Fatalf("unexpected operation id: %q", startupContext.OperationID)
	}

	spanContext := startupContext.RemoteSpanContext()
	if !spanContext.IsValid() {
		t.Fatal("expected valid remote span context")
	}

	if spanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("unexpected trace id: %q", spanContext.TraceID().String())
	}

	if spanContext.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("unexpected span id: %q", spanContext.SpanID().String())
	}
}

func TestStartupContextRemoteSpanContextDoesNotUseCurrentSpan(t *testing.T) {
	currentSpanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1},
		SpanID:  trace.SpanID{1},
	})
	parentCtx := trace.ContextWithSpanContext(context.Background(), currentSpanContext)

	startupContext := StartupContext{
		TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	extractedCtx := startupContext.RemoteParentContext(parentCtx)

	if got := trace.SpanContextFromContext(extractedCtx).TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("unexpected trace id: %q", got)
	}
}
