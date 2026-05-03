/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package telemetry

import (
	"context"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	StartupTracerName = "dcp.startup"

	StartupOperationIDEnvVar = "ASPIRE_STARTUP_OPERATION_ID"
	StartupTraceParentEnvVar = "ASPIRE_STARTUP_TRACEPARENT"
	StartupTraceStateEnvVar  = "ASPIRE_STARTUP_TRACESTATE"

	StartupOperationIDAttribute = "aspire.startup.operation_id"

	ResourceNameAnnotation = "resource-name"
)

type StartupContext struct {
	OperationID string
	TraceParent string
	TraceState  string
}

func StartupContextFromEnvironment() (StartupContext, bool) {
	operationID := os.Getenv(StartupOperationIDEnvVar)
	if operationID == "" {
		return StartupContext{}, false
	}

	return StartupContext{
		OperationID: operationID,
		TraceParent: os.Getenv(StartupTraceParentEnvVar),
		TraceState:  os.Getenv(StartupTraceStateEnvVar),
	}, true
}

func (startupContext StartupContext) Attributes() []attribute.KeyValue {
	if startupContext.OperationID == "" {
		return nil
	}

	return []attribute.KeyValue{
		attribute.String(StartupOperationIDAttribute, startupContext.OperationID),
	}
}

func (startupContext StartupContext) RemoteParentContext(parentCtx context.Context) context.Context {
	if startupContext.TraceParent == "" {
		return parentCtx
	}

	return propagation.TraceContext{}.Extract(parentCtx, propagation.MapCarrier{
		"traceparent": startupContext.TraceParent,
		"tracestate":  startupContext.TraceState,
	})
}

func SetStartupAttributes(ctx context.Context, startupContext StartupContext) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(startupContext.Attributes()...)
}

func StartStartupSpan(parentCtx context.Context, spanName string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	startupContext, ok := StartupContextFromEnvironment()
	if ok {
		if !trace.SpanContextFromContext(parentCtx).IsValid() {
			parentCtx = startupContext.RemoteParentContext(parentCtx)
		}

		attributes = append(startupContext.Attributes(), attributes...)
	}

	return GetTracer(StartupTracerName).Start(parentCtx, spanName, trace.WithAttributes(attributes...))
}

func SetDcpResourceAttributes(ctx context.Context, kind string, namespace string, name string, annotations map[string]string) {
	SetAttribute(ctx, "dcp.resource.kind", kind)
	SetAttribute(ctx, "dcp.resource.name", name)

	if namespace != "" {
		SetAttribute(ctx, "dcp.resource.namespace", namespace)
	}

	if annotations != nil {
		if resourceName := annotations[ResourceNameAnnotation]; resourceName != "" {
			SetAttribute(ctx, "aspire.resource.name", resourceName)
		}
	}
}
