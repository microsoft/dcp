/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/


package telemetry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap/zapcore"
)

func newTraceExporter(logName string) (sdktrace.SpanExporter, error) {
	logLevel, err := logger.GetDiagnosticsLogLevel()

	if err == nil && logLevel == zapcore.DebugLevel {
		logFolder, logFolderErr := logger.EnsureDiagnosticsLogsFolder()

		if logFolderErr != nil {
			return nil, logFolderErr
		}

		telemetryFileName := fmt.Sprintf("%s-%s-telemetry-%s.json", logger.SessionId(), logName, logger.ProcessMomentHash(logger.PlainHash))
		telemetryFile, logFileErr := usvc_io.OpenFile(filepath.Join(logFolder, telemetryFileName), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, osutil.PermissionOnlyOwnerReadWrite)

		if logFileErr != nil {
			return nil, logFileErr
		}

		return stdouttrace.New(stdouttrace.WithPrettyPrint(), stdouttrace.WithWriter(telemetryFile))
	} else {
		return discardExporter{}, nil
	}
}

// newStartupOTLPExporter creates an OTLP/gRPC span exporter using standard OTEL env
// configuration (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS, etc.).
// It is intended only for Aspire-driven startup profiling; callers should gate it on
// IsStartupProfilingEnabled() so we don't open a gRPC channel for normal DCP runs.
func newStartupOTLPExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	return otlptracegrpc.New(ctx)
}

func newMetricExporter() (sdkmetric.Exporter, error) {
	logLevel, err := logger.GetDiagnosticsLogLevel()

	if err == nil && logLevel == zapcore.DebugLevel {
		return stdoutmetric.New()
	} else {
		return discardExporter{}, nil
	}
}

type discardExporter struct{}

func (discardExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return nil
}

func (discardExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return nil
}

func (discardExporter) ForceFlush(context.Context) error {
	return nil
}

func (discardExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (discardExporter) Aggregation(ik sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(ik)
}

func (discardExporter) Temporality(ik sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(ik)
}

var _ sdktrace.SpanExporter = discardExporter{}
var _ sdkmetric.Exporter = discardExporter{}
