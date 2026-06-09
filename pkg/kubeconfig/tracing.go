/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package kubeconfig provides utilities for materializing the kubeconfig file
// that DCP's API server clients (kubectl, controller-runtime, Aspire's hosting
// layer) use to connect to the embedded API server.
//
// This file is the single source of truth for the OpenTelemetry "dcp.kubeconfig.*"
// telemetry surface emitted from this package: every span name is a named
// constant, every span site goes through the traced helper, and the tracer is
// fetched lazily from the global TracerProvider so this package stays free of
// any direct dependency on DCP's internal telemetry wiring.
package kubeconfig

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the OpenTelemetry instrumentation scope name used by this
// package. Exported so DCP's profiling allowlist (see internal/telemetry)
// can reference the same string without hard-coding it in two places. The
// kebab-case form matches the convention used by other DCP tracer scopes
// (controller-common, service-controller, dcp-startup).
const TracerName = "dcp-kubeconfig"

// Span names emitted by this package. Adding a span? Add the constant here
// and reference it at the call site — never embed the literal string.
const (
	spanTlsCertLookup       = "dcp.kubeconfig.tls_cert_lookup"
	spanPreferredHostIps    = "dcp.kubeconfig.preferred_host_ips"
	spanGetFreePort         = "dcp.kubeconfig.get_free_port"
	spanGenerateCertificate = "dcp.kubeconfig.generate_certificate"
	spanGenerateToken       = "dcp.kubeconfig.generate_token"
)

// tracer returns the kubeconfig instrumentation tracer. When no tracer
// provider is registered (the common case for any code that imports this
// package without DCP's telemetry wiring) the returned tracer is a no-op
// and the Start/End pair below is essentially free.
func tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// traced runs fn inside a span named name, returns its result, and ends the
// span. It is the building block for all kubeconfig sub-instrumentation so
// every site looks the same and so that adding error-recording or
// status-propagation later is a single-file change.
//
// Note: internal/telemetry.CallWithTelemetry already does this for code that
// lives under internal/. We intentionally do not reuse it here because
// pkg/kubeconfig is a reusable package — importing internal/telemetry would
// chain in the OTLP/gRPC exporter and break the pkg/ contract that public
// packages do not depend on DCP-internal wiring.
func traced[T any](ctx context.Context, name string, fn func() (T, error)) (T, error) {
	_, span := tracer().Start(ctx, name)
	defer span.End()
	return fn()
}
