// Copyright (c) Microsoft Corporation. All rights reserved.

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
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type TelemetrySystem struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	spanExporter   sdktrace.SpanExporter
	metricExporter sdkmetric.Exporter
}

var instance *TelemetrySystem
var once sync.Once

func GetTelemetrySystem() *TelemetrySystem {
	once.Do(func() {
		logName := filepath.Base(os.Args[0])
		spanExp, err := newTraceExporter(logName)
		if err != nil {
			panic(err)
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(NewSuppressIfSuccessfulSpanProcessor(sdktrace.NewBatchSpanProcessor(spanExp))),
			sdktrace.WithResource(resource.Default()),
		)

		metricExp, err := newMetricExporter()
		if err != nil {
			panic(err)
		}

		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(1*time.Minute)),
			),
		)

		otel.SetTracerProvider(tp)
		// otel.SetMeterProvider(mp) // TODO: Not supported in otel 1.10.0

		instance = &TelemetrySystem{
			TracerProvider: tp,
			MeterProvider:  mp,
			spanExporter:   spanExp,
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
	return errors.Join(
		ts.TracerProvider.Shutdown(ctx),
		ts.MeterProvider.Shutdown(ctx),
		ts.spanExporter.Shutdown(ctx),
		ts.metricExporter.Shutdown(ctx),
	)
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

func HashValue(value string) string {
	hash := sha256.New()
	hash.Write([]byte(value))
	hashBytes := hash.Sum(nil)
	return fmt.Sprintf("%x", hashBytes)
}
