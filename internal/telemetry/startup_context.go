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

	StartupSpanCommand                        = "dcp.command"
	StartupSpanCommandLifetime                = "dcp.command.lifetime"
	StartupSpanStartApiServer                 = "dcp.start_apiserver"
	StartupSpanStartApiServerFork             = "dcp.start_apiserver.fork"
	StartupSpanStartApiServerEnsureKubeconfig = "dcp.start_apiserver.ensure_kubeconfig"
	StartupSpanStartApiServerLifetime         = "dcp.start_apiserver.lifetime"
	StartupSpanRun                            = "dcp.run"
	StartupSpanNotificationSourceCreate       = "dcp.notification_source.create"
	StartupSpanHostedServicesCreate           = "dcp.hosted_services.create"
	StartupSpanApiServerStart                 = "dcp.apiserver.start"
	StartupSpanHostedServicesStart            = "dcp.hosted_services.start"
	StartupSpanExtensionsDiscover             = "dcp.extensions.discover"
	StartupSpanExtensionGetCapabilities       = "dcp.extension.get_capabilities"
	StartupSpanControllersCreateManager       = "dcp.controllers.create_manager"
	StartupSpanControllersRun                 = "dcp.controllers.run"
	StartupSpanControllersManagerStart        = "dcp.controllers.manager.start"
	StartupSpanControllerReconcile            = "dcp.controller.reconcile"
	StartupSpanExecutableManage               = "dcp.executable.manage"
	StartupSpanContainerManage                = "dcp.container.manage"
	StartupSpanServiceEnsureEffectiveAddress  = "dcp.service.ensure_effective_address"

	StartupEventCommandRootCommandCreating      = "dcp.command.root_command.creating"
	StartupEventCommandRootCommandCreated       = "dcp.command.root_command.created"
	StartupEventCommandExecuteStart             = "dcp.command.execute.start"
	StartupEventCommandExecuteFailed            = "dcp.command.execute.failed"
	StartupEventCommandExecuteSucceeded         = "dcp.command.execute.succeeded"
	StartupEventStartApiServerMonitorConfigured = "dcp.start_apiserver.monitor_configured"
	StartupEventStartApiServerForkStarting      = "dcp.start_apiserver.fork.starting"
	StartupEventStartApiServerForkStarted       = "dcp.start_apiserver.fork.started"
	StartupEventStartApiServerRootDirResolving  = "dcp.start_apiserver.root_dir.resolving"
	StartupEventStartApiServerKubeconfigReady   = "dcp.start_apiserver.kubeconfig_ready"
	StartupEventRunApiServerStarting            = "dcp.run.apiserver.starting"
	StartupEventRunApiServerStarted             = "dcp.apiserver.started"
	StartupEventRunHostedServicesStarting       = "dcp.run.hosted_services.starting"
	StartupEventRunHostedServicesStarted        = "dcp.hosted_services.started"
	StartupEventHostedServiceError              = "dcp.hosted_service.error"
	StartupEventRunShutdownRequested            = "dcp.run.shutdown_requested"
	StartupEventRunApiServerStopped             = "dcp.run.apiserver_stopped"

	StartupAttributeProcessPID                = "process.pid"
	StartupAttributeProcessExecutableName     = "process.executable.name"
	StartupAttributeCommandArgumentCount      = "dcp.command.argument_count"
	StartupAttributeCommandName               = "dcp.command.name"
	StartupAttributeCommandExitCode           = "dcp.command.exit_code"
	StartupAttributeDetach                    = "dcp.detach"
	StartupAttributeServerOnly                = "dcp.server_only"
	StartupAttributeForkArgumentCount         = "dcp.fork.argument_count"
	StartupAttributeForkPID                   = "dcp.fork.pid"
	StartupAttributeRootDirectoryBasename     = "dcp.root_directory.basename"
	StartupAttributeExtensionsCount           = "dcp.extensions.count"
	StartupAttributeExtensionsDirectoryCount  = "dcp.extensions.directory_count"
	StartupAttributeInvocationFlagsCount      = "dcp.invocation_flags.count"
	StartupAttributeControllerExtensionsCount = "dcp.controller_extensions.count"
	StartupAttributeHostedServicesCount       = "dcp.hosted_services.count"
	StartupAttributeNotificationSourceCreated = "dcp.notification_source.created"
	StartupAttributeResourceCleanup           = "dcp.resource_cleanup"
	StartupAttributeExtensionExecutableName   = "dcp.extension.executable_name"
	StartupAttributeExtensionID               = "dcp.extension.id"
	StartupAttributeExtensionName             = "dcp.extension.name"
	StartupAttributeExtensionCapabilityCount  = "dcp.extension.capability_count"
	StartupAttributeControllerKind            = "dcp.controller.kind"
	StartupAttributeResourceKind              = "dcp.resource.kind"
	StartupAttributeResourceName              = "dcp.resource.name"
	StartupAttributeResourceNamespace         = "dcp.resource.namespace"
	StartupAttributeResourceState             = "dcp.resource.state"
	StartupAttributeResourceFinalState        = "dcp.resource.final_state"
	StartupAttributeExecutableTargetState     = "dcp.executable.target_state"
	StartupAttributeExecutableRunState        = "dcp.executable.run_state"
	StartupAttributeContainerTargetState      = "dcp.container.target_state"
	StartupAttributeContainerRunState         = "dcp.container.run_state"
	StartupAttributeServiceFinalState         = "dcp.service.final_state"
	StartupAttributeServiceEffectivePort      = "dcp.service.effective_port"
	StartupAttributeServicePreviousState      = "dcp.service.previous_state"
	StartupAttributeServiceAddressAllocation  = "dcp.service.address_allocation_mode"
	StartupAttributeAspireResourceName        = "aspire.resource.name"
	StartupAttributeAspireLinkType            = "aspire.link.type"

	StartupOperationIDEnvVar = "ASPIRE_STARTUP_OPERATION_ID"
	StartupTraceParentEnvVar = "ASPIRE_STARTUP_TRACEPARENT"
	StartupTraceStateEnvVar  = "ASPIRE_STARTUP_TRACESTATE"

	StartupOperationIDAttribute = "aspire.startup.operation_id"

	StartupOperationIDAnnotation = "aspire-startup-operation-id"
	StartupTraceParentAnnotation = "aspire-startup-traceparent"
	StartupTraceStateAnnotation  = "aspire-startup-tracestate"

	HostingDcpCreateObjectIDAttribute      = "aspire.hosting.dcp.create_object.id"
	HostingDcpCreateObjectKindAttribute    = "aspire.hosting.dcp.create_object.kind"
	HostingDcpCreateObjectNameAttribute    = "aspire.hosting.dcp.create_object.name"
	HostingDcpCreateObjectTraceIDAttribute = "aspire.hosting.dcp.create_object.trace_id"
	HostingDcpCreateObjectSpanIDAttribute  = "aspire.hosting.dcp.create_object.span_id"

	ResourceNameAnnotation = "resource-name"
)

type StartupSpan struct {
	span trace.Span
}

func (startupSpan StartupSpan) End() {
	startupSpan.span.End()
}

func (startupSpan StartupSpan) SetError(err error) {
	SetError(startupSpan.span, err)
}

func (startupSpan StartupSpan) EndWithError(err error) {
	startupSpan.SetError(err)
	startupSpan.End()
}

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

func StartupContextFromAnnotations(annotations map[string]string) (StartupContext, bool) {
	if len(annotations) == 0 {
		return StartupContext{}, false
	}

	startupContext := StartupContext{
		OperationID: annotations[StartupOperationIDAnnotation],
		TraceParent: annotations[StartupTraceParentAnnotation],
		TraceState:  annotations[StartupTraceStateAnnotation],
	}

	return startupContext, startupContext.OperationID != "" || startupContext.TraceParent != ""
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

func (startupContext StartupContext) RemoteSpanContext() trace.SpanContext {
	if startupContext.TraceParent == "" {
		return trace.SpanContext{}
	}

	return trace.SpanContextFromContext(startupContext.RemoteParentContext(context.Background()))
}

func SetStartupAttributes(ctx context.Context, startupContext StartupContext) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(startupContext.Attributes()...)
}

func StartStartupSpan(parentCtx context.Context, spanName string, attributes ...attribute.KeyValue) (context.Context, StartupSpan) {
	startupContext, ok := StartupContextFromEnvironment()
	if ok {
		if !trace.SpanContextFromContext(parentCtx).IsValid() {
			parentCtx = startupContext.RemoteParentContext(parentCtx)
		}

		attributes = append(startupContext.Attributes(), attributes...)
	}

	ctx, span := GetTracer(StartupTracerName).Start(parentCtx, spanName, trace.WithAttributes(attributes...))
	return ctx, StartupSpan{span: span}
}

func SetDcpResourceAttributes(ctx context.Context, kind string, namespace string, name string, annotations map[string]string) {
	SetAttribute(ctx, StartupAttributeResourceKind, kind)
	SetAttribute(ctx, StartupAttributeResourceName, name)

	if namespace != "" {
		SetAttribute(ctx, StartupAttributeResourceNamespace, namespace)
	}

	if annotations != nil {
		if resourceName := annotations[ResourceNameAnnotation]; resourceName != "" {
			SetAttribute(ctx, StartupAttributeAspireResourceName, resourceName)
		}

		SetAspireStartupAnnotationAttributes(ctx, annotations)
		if _, ok := StartupContextFromAnnotations(annotations); ok {
			SetAttribute(ctx, HostingDcpCreateObjectIDAttribute, dcpCreateObjectID(kind, namespace, name))
			SetAttribute(ctx, HostingDcpCreateObjectKindAttribute, kind)
			SetAttribute(ctx, HostingDcpCreateObjectNameAttribute, name)
		}
	}
}

func SetAspireStartupAnnotationAttributes(ctx context.Context, annotations map[string]string) {
	startupContext, ok := StartupContextFromAnnotations(annotations)
	if !ok {
		return
	}

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(startupContext.Attributes()...)

	if spanContext := startupContext.RemoteSpanContext(); spanContext.IsValid() {
		span.SetAttributes(
			attribute.String(HostingDcpCreateObjectTraceIDAttribute, spanContext.TraceID().String()),
			attribute.String(HostingDcpCreateObjectSpanIDAttribute, spanContext.SpanID().String()),
		)
		span.AddLink(trace.Link{
			SpanContext: spanContext,
			Attributes: []attribute.KeyValue{
				attribute.String(StartupAttributeAspireLinkType, "hosting.dcp.create_object"),
			},
		})
	}
}

func dcpCreateObjectID(kind string, namespace string, name string) string {
	if namespace == "" {
		return kind + "/" + name
	}

	return kind + "/" + namespace + "/" + name
}
