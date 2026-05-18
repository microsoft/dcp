/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/


package telemetry

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/microsoft/dcp/pkg/osutil"
)

type TelemetrySystem struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	spanExporter   sdktrace.SpanExporter
	otlpExporter   sdktrace.SpanExporter
	metricExporter sdkmetric.Exporter
}

var instance *TelemetrySystem
var once sync.Once

func GetTelemetrySystem() *TelemetrySystem {
	once.Do(func() {
		logName := filepath.Base(os.Args[0])
		if osutil.IsWindows() {
			logName = logName[:len(logName)-len(filepath.Ext(logName))]
		}
		spanExp, err := newTraceExporter(logName)
		if err != nil {
			// TODO: report error
			spanExp = discardExporter{}
		}

		// Resource: identify spans as belonging to the DCP service. When startup profiling
		// is on, attach the Aspire profiling session id so Aspire can correlate cross-process
		// startup traces emitted from DCP with the surrounding aspire-cli / aspire-apphost ones.
		resAttrs := []attribute.KeyValue{
			semconv.ServiceName("dcp"),
		}
		if sid := ProfilingSessionId(); sid != "" {
			resAttrs = append(resAttrs, attribute.String("aspire.profiling.session_id", sid))
		}
		res, mergeErr := resource.Merge(
			resource.Default(),
			resource.NewWithAttributes(semconv.SchemaURL, resAttrs...),
		)
		if mergeErr != nil {
			// Fall back to schema-less attributes so we never lose service.name.
			res = resource.NewSchemaless(resAttrs...)
		}

		tpOptions := []sdktrace.TracerProviderOption{
			sdktrace.WithSpanProcessor(NewSuppressIfSuccessfulSpanProcessor(sdktrace.NewBatchSpanProcessor(spanExp))),
			sdktrace.WithResource(res),
		}

		// When Aspire-driven startup profiling is enabled, attach a second batch processor
		// pointed at the configured OTLP endpoint. The processor is wrapped in a
		// scope allowlist so that ONLY startup-related spans are sent: controller /
		// runtime spans on the same TracerProvider must not be exported, because
		// enabling Aspire startup profiling is supposed to capture startup costs — not
		// silently exfiltrate application-runtime telemetry for the lifetime of the
		// DCP process. BatchTimeout is intentionally short (250ms) so spans appear
		// promptly even though we keep running past startup.
		var otlpExp sdktrace.SpanExporter
		if IsStartupProfilingEnabled() {
			otlpCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			exp, otlpErr := newStartupOtlpExporter(otlpCtx)
			if otlpErr == nil {
				otlpExp = exp
				tpOptions = append(tpOptions,
					sdktrace.WithSpanProcessor(newScopeAllowlistSpanProcessor(
						sdktrace.NewBatchSpanProcessor(
							exp,
							sdktrace.WithBatchTimeout(250*time.Millisecond),
						),
						startupExportedScopes...,
					)),
				)
			}
		}

		tp := sdktrace.NewTracerProvider(tpOptions...)

		metricExp, err := newMetricExporter()
		if err != nil {
			// TODO: report error
			metricExp = discardExporter{}
		}

		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(1*time.Minute)),
			),
		)

		otel.SetTracerProvider(tp)
		// otel.SetMeterProvider(mp) // TODO: Not supported in otel 1.10.0

		// Install the W3C TraceContext propagator so injected spans/baggage work as expected
		// across DCP subprocesses and the surrounding Aspire orchestration.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))

		instance = &TelemetrySystem{
			TracerProvider: tp,
			MeterProvider:  mp,
			spanExporter:   spanExp,
			otlpExporter:   otlpExp,
			metricExporter: metricExp,
		}
	})

	return instance
}

func GetTracer(tracerName string) trace.Tracer {
	ts := GetTelemetrySystem()
	return ts.TracerProvider.Tracer(tracerName)
}

func GetMeter(meterName string) metric.Meter {
	ts := GetTelemetrySystem()
	return ts.MeterProvider.Meter(meterName)
}

func (ts TelemetrySystem) Shutdown(ctx context.Context) error {
	errs := []error{
		ts.TracerProvider.Shutdown(ctx),
		ts.MeterProvider.Shutdown(ctx),
		ts.spanExporter.Shutdown(ctx),
		ts.metricExporter.Shutdown(ctx),
	}
	if ts.otlpExporter != nil {
		errs = append(errs, ts.otlpExporter.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func CallWithTelemetryOnErrorOnly[TResult any](tracer trace.Tracer, spanName string, parentCtx context.Context, fn func(ctx context.Context) (TResult, error)) (TResult, error) {
	spanCtx, span := tracer.Start(parentCtx, spanName, trace.WithAttributes(attribute.Bool(suppressIfSuccessful, true)))
	defer span.End()

	result, err := fn(spanCtx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}

func CallWithTelemetry[TResult any](tracer trace.Tracer, spanName string, parentCtx context.Context, fn func(ctx context.Context) (TResult, error)) (TResult, error) {
	spanCtx, span := tracer.Start(parentCtx, spanName)
	defer span.End()

	result, err := fn(spanCtx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}

func CallWithTelemetryNoResult(tracer trace.Tracer, spanName string, parentCtx context.Context, fn func(ctx context.Context) error) error {
	spanCtx, span := tracer.Start(parentCtx, spanName)
	defer span.End()

	err := fn(spanCtx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

type TelemetryAttribute interface {
	int | int64 | bool | float64 | string | []int | []int64 | []bool | []float64 | []string
}

func SetAttribute[T TelemetryAttribute](ctx context.Context, key string, value T) {
	span := trace.SpanFromContext(ctx)

	switch v := (any)(value).(type) {
	case int:
		span.SetAttributes(attribute.Int(key, v))
	case int64:
		span.SetAttributes(attribute.Int64(key, v))
	case bool:
		span.SetAttributes(attribute.Bool(key, v))
	case float64:
		span.SetAttributes(attribute.Float64(key, v))
	case string:
		span.SetAttributes(attribute.String(key, v))
	case []int:
		span.SetAttributes(attribute.IntSlice(key, v))
	case []int64:
		span.SetAttributes(attribute.Int64Slice(key, v))
	case []bool:
		span.SetAttributes(attribute.BoolSlice(key, v))
	case []float64:
		span.SetAttributes(attribute.Float64Slice(key, v))
	case []string:
		span.SetAttributes(attribute.StringSlice(key, v))
	default:
		// This should never happen
		fmt.Printf("unknown telemetry type for key %s", key)
	}
}

func AddEvent(ctx context.Context, name string, options ...trace.EventOption) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent(name, options...)
}

var hash = sha256.New()

func HashValue(value string) string {
	hash.Reset()

	hash.Write([]byte(value))
	hashBytes := hash.Sum(nil)
	return fmt.Sprintf("%x", hashBytes)
}
