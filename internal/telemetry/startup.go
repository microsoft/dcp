/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package telemetry implements DCP's OpenTelemetry wiring, including the
// Aspire-driven startup-profiling pipeline that exports a bounded set of
// startup spans over OTLP/gRPC when the surrounding orchestrator opts in.
//
// This package owns the export *plumbing*: the OTLP/gRPC exporter, the
// scope-allowlist span processor, the tracer registry, the env-var contract
// with Aspire, and ForceFlush. The *vocabulary* of startup spans (span,
// event, and attribute names) lives in the sibling package
// internal/telemetry/startupspans, which has its own doc explaining the
// rationale for that split.
package telemetry

import (
	"cmp"
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/microsoft/dcp/pkg/kubeconfig"
	"github.com/microsoft/dcp/pkg/osutil"
)

// Environment variables consumed by the startup-profiling pipeline. These mirror
// the names Aspire sets so DCP spans land in the same trace as the surrounding
// aspire.hosting.dcp.* spans without any explicit handshake. The constant
// identifier intentionally matches the env var value verbatim, mirroring the
// convention used by DCP_STATE_STORE_PATH, DCP_PERF_TRACE, DCP_LOG_SOCKET, etc.
// across this repo.
const (
	// ASPIRE_PROFILING_ENABLED enables Aspire-style startup profiling.
	ASPIRE_PROFILING_ENABLED = "ASPIRE_PROFILING_ENABLED"

	// OTEL_EXPORTER_OTLP_* follow the OTel spec for OTLP exporter configuration.
	OTEL_EXPORTER_OTLP_ENDPOINT        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	OTEL_EXPORTER_OTLP_PROTOCOL        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	OTEL_EXPORTER_OTLP_TRACES_PROTOCOL = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"

	// ASPIRE_STARTUP_TRACEPARENT / ASPIRE_STARTUP_TRACESTATE carry the W3C trace
	// context injected by Aspire so DCP spans become children of Aspire's outer
	// activity.
	ASPIRE_STARTUP_TRACEPARENT = "ASPIRE_STARTUP_TRACEPARENT"
	ASPIRE_STARTUP_TRACESTATE  = "ASPIRE_STARTUP_TRACESTATE"

	// ASPIRE_PROFILING_SESSION_ID is propagated as a resource attribute so
	// Aspire's per-run capture can group spans across processes.
	ASPIRE_PROFILING_SESSION_ID = "ASPIRE_PROFILING_SESSION_ID"

	// StartupTracerName is the OpenTelemetry instrumentation scope name used
	// for DCP startup spans. The kebab-case form matches the convention used
	// by other DCP tracers in this repo (controller-common, service-controller).
	// Aspire's profile viewer displays whatever scope name we set, so consumer
	// convention doesn't force a different choice here.
	StartupTracerName = "dcp-startup"
)

// startupExportedScopes is the explicit allowlist of instrumentation-scope names
// that may be exported through the Aspire-driven OTLP exporter. Anything outside
// this list (e.g. controller-common, service-controller) is dropped before
// reaching the OTLP endpoint so enabling startup profiling never silently turns
// on a continuous telemetry stream of application-runtime spans.
var startupExportedScopes = []string{
	StartupTracerName,
	kubeconfig.TracerName,
}

// noopTracer is returned by StartupTracer when profiling is disabled so call sites
// can use the standard `tracer.Start(...) / defer span.End()` pattern without any
// `if enabled` guards.
var noopTracer = noop.NewTracerProvider().Tracer(StartupTracerName)

// IsStartupProfilingEnabled reports whether Aspire-compatible startup profiling is
// turned on for this process. It checks the opt-in env var, that an OTLP endpoint
// is configured, and that the selected protocol is gRPC (the only protocol the
// DCP exporter speaks). The result is computed once and cached.
var IsStartupProfilingEnabled = sync.OnceValue(func() bool {
	if !osutil.EnvVarSwitchEnabled(ASPIRE_PROFILING_ENABLED) {
		return false
	}
	if cmp.Or(os.Getenv(OTEL_EXPORTER_OTLP_TRACES_ENDPOINT), os.Getenv(OTEL_EXPORTER_OTLP_ENDPOINT)) == "" {
		return false
	}
	protocol := strings.ToLower(cmp.Or(os.Getenv(OTEL_EXPORTER_OTLP_TRACES_PROTOCOL), os.Getenv(OTEL_EXPORTER_OTLP_PROTOCOL)))
	return protocol == "" || protocol == "grpc"
})

// ProfilingSessionId returns the propagated Aspire profiling session id, if any.
var ProfilingSessionId = sync.OnceValue(func() string {
	return os.Getenv(ASPIRE_PROFILING_SESSION_ID)
})

// ExtractStartupTraceContext extracts a W3C traceparent from the environment so
// DCP startup spans become children of Aspire's outer startup activity. If no
// traceparent is present the parent context is returned unchanged.
func ExtractStartupTraceContext(parent context.Context) context.Context {
	traceparent := os.Getenv(ASPIRE_STARTUP_TRACEPARENT)
	if traceparent == "" {
		return parent
	}
	carrier := propagation.MapCarrier{"traceparent": traceparent}
	if ts := os.Getenv(ASPIRE_STARTUP_TRACESTATE); ts != "" {
		carrier["tracestate"] = ts
	}
	return propagation.TraceContext{}.Extract(parent, carrier)
}

// StartupTracer returns the tracer used for DCP startup spans. When startup
// profiling is disabled a no-op tracer is returned, so callers can use the
// standard OTEL pattern without `if enabled` guards.
func StartupTracer() trace.Tracer {
	if !IsStartupProfilingEnabled() {
		return noopTracer
	}
	return GetTracer(StartupTracerName)
}

// ForceFlushStartup synchronously flushes any pending startup spans with a bounded
// timeout so they appear in Aspire's profile capture even though DCP keeps running.
// When profiling is disabled this is a no-op.
func ForceFlushStartup(log logr.Logger) {
	if !IsStartupProfilingEnabled() {
		return
	}
	ts := GetTelemetrySystem()
	if ts == nil || ts.TracerProvider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ts.TracerProvider.ForceFlush(ctx); err != nil {
		log.V(1).Info("Failed to flush DCP startup telemetry", "error", err)
	}
}
