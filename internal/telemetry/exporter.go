/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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

const (
	otelExporterOtlpEndpointEnvVar       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	otelExporterOtlpTracesEndpointEnvVar = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	otelExporterOtlpHeadersEnvVar        = "OTEL_EXPORTER_OTLP_HEADERS"
	otelExporterOtlpTracesHeadersEnvVar  = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	otelExporterOtlpProtocolEnvVar       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	aspireDashboardOtlpEndpointEnvVar    = "ASPIRE_DASHBOARD_OTLP_ENDPOINT_URL"
)

func newTraceExporter(logName string) (sdktrace.SpanExporter, error) {
	if otlpEndpoint := getOtlpEndpoint(); otlpEndpoint != "" {
		return newOtlpTraceExporter(context.Background(), otlpEndpoint)
	}

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

func getOtlpEndpoint() string {
	if endpoint := os.Getenv(otelExporterOtlpTracesEndpointEnvVar); endpoint != "" {
		return endpoint
	}

	if endpoint := os.Getenv(otelExporterOtlpEndpointEnvVar); endpoint != "" {
		return endpoint
	}

	return os.Getenv(aspireDashboardOtlpEndpointEnvVar)
}

func newOtlpTraceExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	protocol := strings.ToLower(os.Getenv(otelExporterOtlpProtocolEnvVar))
	if protocol != "" && protocol != "grpc" {
		return nil, fmt.Errorf("unsupported OTLP trace exporter protocol %q", protocol)
	}

	options := []otlptracegrpc.Option{}
	if parsedEndpoint, err := url.Parse(endpoint); err == nil && parsedEndpoint.Host != "" {
		options = append(options, otlptracegrpc.WithEndpoint(parsedEndpoint.Host))
		if parsedEndpoint.Scheme == "http" {
			options = append(options, otlptracegrpc.WithInsecure())
		}
	} else {
		options = append(options, otlptracegrpc.WithEndpoint(endpoint))
	}

	if headers := getOtlpHeaders(); len(headers) > 0 {
		options = append(options, otlptracegrpc.WithHeaders(headers))
	}

	return otlptracegrpc.New(ctx, options...)
}

func getOtlpHeaders() map[string]string {
	headers := parseOtlpHeaders(os.Getenv(otelExporterOtlpTracesHeadersEnvVar))
	if len(headers) > 0 {
		return headers
	}

	return parseOtlpHeaders(os.Getenv(otelExporterOtlpHeadersEnvVar))
}

func parseOtlpHeaders(rawHeaders string) map[string]string {
	headers := map[string]string{}
	for _, pair := range strings.Split(rawHeaders, ",") {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}

		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		headers[key] = strings.TrimSpace(value)
	}

	return headers
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
