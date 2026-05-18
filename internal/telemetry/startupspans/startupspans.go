/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package startupspans is the single source of truth for the DCP "dcp.startup.*"
// telemetry surface that powers Aspire-driven startup profiling.
//
// All span names, event names, and attribute keys are declared as exported
// constants here so call sites never embed magic strings. A small set of thin
// helpers (Begin, BeginStartup, BeginCmdInit, BeginParentFork,
// BeginApiServerKubeconfigSave, RecordApiServerReady) covers the cases where
// the same attribute layout would otherwise be repeated at every call site.
// For the fn-shaped pattern, callers use telemetry.CallWithTelemetry with
// Tracer().
//
// When startup profiling is disabled telemetry.StartupTracer returns a no-op
// tracer, so the constants and helpers in this package are effectively free
// at the call site.
//
// # Why is this its own package, separate from internal/telemetry?
//
// internal/telemetry is the export *plumbing* — OTLP/gRPC exporter, scope
// allowlist, span processors, propagation, tracer registry, env-var contract
// with Aspire. It is imported by controllers and other long-running subsystems
// that emit runtime metrics/traces.
//
// internal/telemetry/startupspans is the *vocabulary* — the catalog of span,
// event, and attribute names emitted during startup, plus thin call-site
// helpers. It exists as its own package for three reasons:
//
//  1. Narrower imports at the call site. internal/apiserver only needs
//     startupspans, so it does not transitively depend on the OTLP/gRPC
//     client, propagators, and processor wiring just to reference span names.
//  2. Single-purpose discovery surface. "Add a new dcp.startup.* span" means
//     opening one small file with all names, attrs, events, and the helpers
//     that use them — no need to scroll past export configuration.
//  3. Different rates of change. The vocabulary is the contract Aspire reads
//     and changes only when instrumentation is added or renamed; the plumbing
//     changes for unrelated reasons (exporter version, processor config,
//     env-var compatibility). Keeping them apart insulates the contract from
//     plumbing churn.
package startupspans

import (
	"context"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/microsoft/dcp/internal/telemetry"
)

// Span names. Adding a span? Add the constant here and reference it at the
// call site — never embed the literal string.
const (
	SpanStartup                 = "dcp.startup"
	SpanCmdInit                 = "dcp.startup.cmd_init"
	SpanParentFork              = "dcp.startup.parent_fork"
	SpanEnsureKubeconfig        = "dcp.startup.ensure_kubeconfig_data"
	SpanGetExtensions           = "dcp.startup.get_extensions"
	SpanApiServerComputeOptions = "dcp.startup.api_server.compute_options"
	SpanApiServerBuildConfig    = "dcp.startup.api_server.build_config"
	SpanApiServerKubeconfigSave = "dcp.startup.api_server.kubeconfig_save"
	SpanApiServerRunServer      = "dcp.startup.api_server.run_server"
)

// Event names emitted with AddEvent on an active span.
const EventApiServerReady = "dcp.startup.api_server.ready"

// Attribute keys. These follow OTel semantic conventions where they exist
// (process.*) and use the "dcp." prefix for DCP-specific attributes.
const (
	AttrCommand        = "dcp.command"
	AttrProcessPid     = "process.pid"
	AttrProcessPpid    = "process.ppid"
	AttrServerOnly     = "dcp.server_only"
	AttrDetachParent   = "dcp.detach_parent"
	AttrChildPid       = "dcp.child.pid"
	AttrExtensionCount = "dcp.extension_count"
	AttrKubeconfigPath = "dcp.kubeconfig.path"
	AttrBindAddress    = "dcp.server.bind_address"
	AttrBindPort       = "dcp.server.bind_port"
)

// Tracer returns the startup-profiling tracer. Exported so call sites that
// don't fit one of the helpers below can still use `tracer.Start(ctx, SpanX)`
// with the constants above.
func Tracer() trace.Tracer { return telemetry.StartupTracer() }

// Begin opens a span with the given name. It is shorthand for
// `Tracer().Start(ctx, name, opts...)` and exists so call sites don't have
// to import both this package and go.opentelemetry.io/otel/trace just to
// start an attribute-less span.
func Begin(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// BeginStartup opens the root "dcp.startup" span for the API-server-hosting
// process. The returned span is expected to be ended as soon as the API
// server listener is up — that is when Aspire's `ensure_kubernetes_client`
// wait completes — not when the process exits.
func BeginStartup(ctx context.Context, serverOnly bool) (context.Context, trace.Span) {
	return Begin(ctx, SpanStartup, trace.WithAttributes(
		attribute.Int(AttrProcessPid, os.Getpid()),
		attribute.Int(AttrProcessPpid, os.Getppid()),
		attribute.Bool(AttrServerOnly, serverOnly),
	))
}

// BeginCmdInit wraps the cobra/klog/controller-runtime wiring step that
// happens before any RunE fires.
func BeginCmdInit(ctx context.Context, command string) (context.Context, trace.Span) {
	return Begin(ctx, SpanCmdInit, trace.WithAttributes(
		attribute.String(AttrCommand, command),
		attribute.Int(AttrProcessPid, os.Getpid()),
	))
}

// BeginParentFork wraps the spawn step in the --detach path. Parent and child
// end up as siblings under the same outer Aspire trace because they read the
// same ASPIRE_STARTUP_TRACEPARENT.
func BeginParentFork(ctx context.Context) (context.Context, trace.Span) {
	return Begin(ctx, SpanParentFork, trace.WithAttributes(
		attribute.Int(AttrProcessPid, os.Getpid()),
		attribute.Bool(AttrDetachParent, true),
	))
}

// BeginApiServerKubeconfigSave wraps the durable write of the kubeconfig
// file — the moment Aspire's `aspire.dcp.kubeconfig.file_wait_ms` ends and
// the single most useful span for diagnosing startup latency.
func BeginApiServerKubeconfigSave(ctx context.Context, path string) (context.Context, trace.Span) {
	return Begin(ctx, SpanApiServerKubeconfigSave, trace.WithAttributes(
		attribute.String(AttrKubeconfigPath, path),
	))
}

// SetChildPid attaches the spawned child PID to a parent-fork span once it
// is known.
func SetChildPid(span trace.Span, pid int) {
	span.SetAttributes(attribute.Int(AttrChildPid, pid))
}

// SetExtensionCount records how many extensions were discovered. Call from
// inside the GetExtensions span so the attribute attaches to the right span.
func SetExtensionCount(ctx context.Context, n int) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int(AttrExtensionCount, n))
}

// RecordApiServerReady emits the readiness event on the active span. Aspire
// consumers key off this event to know the listener is up before they try
// to connect, so the event must be emitted from inside the SpanApiServerRunServer
// span (i.e. before that span is ended).
func RecordApiServerReady(ctx context.Context, bindAddress string, bindPort int) {
	trace.SpanFromContext(ctx).AddEvent(EventApiServerReady, trace.WithAttributes(
		attribute.String(AttrBindAddress, bindAddress),
		attribute.Int(AttrBindPort, bindPort),
	))
}
