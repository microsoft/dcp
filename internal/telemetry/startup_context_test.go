/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestStartupContextFromEnvironmentReturnsFalseWhenOperationIDMissing(t *testing.T) {
	t.Setenv(StartupOperationIDEnvVar, "")
	t.Setenv(StartupTraceParentEnvVar, "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01")

	_, ok := StartupContextFromEnvironment()
	if ok {
		t.Fatal("expected startup context to be missing")
	}
}

func TestStartupContextFromEnvironmentReadsCorrelationValues(t *testing.T) {
	t.Setenv(StartupOperationIDEnvVar, "operation-1")
	t.Setenv(StartupTraceParentEnvVar, "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01")
	t.Setenv(StartupTraceStateEnvVar, "state-1")

	startupContext, ok := StartupContextFromEnvironment()
	if !ok {
		t.Fatal("expected startup context")
	}

	if startupContext.OperationID != "operation-1" {
		t.Fatalf("unexpected operation id: %q", startupContext.OperationID)
	}
	if startupContext.TraceParent != "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01" {
		t.Fatalf("unexpected traceparent: %q", startupContext.TraceParent)
	}
	if startupContext.TraceState != "state-1" {
		t.Fatalf("unexpected tracestate: %q", startupContext.TraceState)
	}
}

func TestRemoteParentContextExtractsTraceContext(t *testing.T) {
	startupContext := StartupContext{
		OperationID: "operation-1",
		TraceParent: "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01",
		TraceState:  "state-1",
	}

	ctx := startupContext.RemoteParentContext(context.Background())
	spanContext := trace.SpanContextFromContext(ctx)

	if !spanContext.IsRemote() {
		t.Fatal("expected remote span context")
	}
	if spanContext.TraceID().String() != "0102030405060708090a0b0c0d0e0f10" {
		t.Fatalf("unexpected trace id: %q", spanContext.TraceID().String())
	}
	if spanContext.SpanID().String() != "1112131415161718" {
		t.Fatalf("unexpected span id: %q", spanContext.SpanID().String())
	}
}
