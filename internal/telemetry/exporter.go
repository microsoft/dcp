package telemetry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap/zapcore"
)

func newTraceExporter(logName string) (sdktrace.SpanExporter, error) {
	logLevel, err := logger.GetDebugLogLevel()

	if err == nil && logLevel == zapcore.DebugLevel {
		logFolder, err := logger.EnsureDetailedLogsFolder()

		if err != nil {
			return nil, err
		}

		telemetryFileName := fmt.Sprintf("telemetry-%s-%d-%d.json", logName, time.Now().Unix(), os.Getpid())
		telemetryFile, err := io.OpenFile(filepath.Join(logFolder, telemetryFileName), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_TRUNC, osutil.PermissionOnlyOwnerReadWrite)

		if err != nil {
			return nil, err
		}

		return stdouttrace.New(stdouttrace.WithPrettyPrint(), stdouttrace.WithWriter(telemetryFile))
	} else {
		return discardExporter{}, nil
	}
}

func newMetricExporter() (sdkmetric.Exporter, error) {
	logLevel, err := logger.GetDebugLogLevel()

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

func (discardExporter) Export(context.Context, metricdata.ResourceMetrics) error {
	return nil
}

func (discardExporter) ForceFlush(context.Context) error {
	return nil
}

func (discardExporter) Shutdown(ctx context.Context) error {
	return nil
}
