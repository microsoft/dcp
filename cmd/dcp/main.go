/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

//go:generate goversioninfo

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/dcp/commands"
	"github.com/microsoft/dcp/internal/telemetry"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	errCommand = 1
	errSetup   = 2
	errPanic   = 3
)

func main() {
	logName := "dcp"
	if len(os.Args) > 1 && osutil.HasOnlyValidFilenameChars(os.Args[1]) && os.Args[1][0] != '-' {
		// Use the command name as part of the log file name, instead of just "dcp", which is the same for all invocations.
		logName = os.Args[1]
	}
	log := logger.New(logName).
		WithFilterSink(logger.MacOsProcErrorLogFilter, 1).
		WithName("dcp")

	defer func() {
		panicErr := resiliency.MakePanicError(recover(), log.Logger)
		if panicErr != nil {
			os.Stderr.WriteString(panicErr.Error() + string(osutil.LineSep()))
			log.Flush()
			os.Exit(errPanic)
		}
	}()

	ctx := kubeapiserver.SetupSignalContext()

	// Ingest any W3C traceparent propagated by an outer orchestrator (e.g. Aspire's
	// hosting layer) so DCP startup spans become children of that activity. This is
	// always cheap — it returns ctx unchanged when no traceparent env var is set.
	ctx = telemetry.ExtractStartupTraceContext(ctx)

	root, err := buildRootCmd(ctx, log, logName)
	if err != nil {
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	if err != nil {
		cmdutil.ErrorExit(log, err, errCommand)
	} else {
		log.Flush()
	}
}

// buildRootCmd constructs the root cobra command, wrapped in a short startup span so
// that profiling captures the time spent in cobra / klog / controller-runtime wiring
// before the leaf command's RunE runs. When startup profiling is disabled the
// returned tracer is a no-op, so this is a couple of nanoseconds either way.
func buildRootCmd(ctx context.Context, log *logger.Logger, logName string) (*cobra.Command, error) {
	_, span := telemetry.StartupTracer().Start(ctx, "dcp.startup.cmd_init",
		trace.WithAttributes(
			attribute.String("dcp.command", logName),
			attribute.Int("process.pid", os.Getpid()),
		),
	)
	defer span.End()
	return commands.NewRootCmd(log)
}
