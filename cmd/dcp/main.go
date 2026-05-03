/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

//go:generate goversioninfo

import (
	"context"
	"os"
	"sync"
	"time"

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

	telemetrySystem := telemetry.GetTelemetrySystem()
	var shutdownTelemetryOnce sync.Once
	shutdownTelemetry := func() {
		shutdownTelemetryOnce.Do(func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = telemetrySystem.Shutdown(shutdownCtx)
		})
	}
	defer shutdownTelemetry()

	ctx, commandSpan := telemetry.StartStartupSpan(ctx, "dcp.command")
	if len(os.Args) > 1 && os.Args[1] != "" && os.Args[1][0] != '-' {
		telemetry.SetAttribute(ctx, "dcp.command.name", os.Args[1])
	}
	spanEnded := false
	endCommandSpan := func() {
		if !spanEnded {
			commandSpan.End()
			spanEnded = true
		}
	}
	defer endCommandSpan()

	root, err := commands.NewRootCmd(log)
	if err != nil {
		telemetry.SetError(commandSpan, err)
		endCommandSpan()
		shutdownTelemetry()
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	if err != nil {
		telemetry.SetError(commandSpan, err)
		endCommandSpan()
		shutdownTelemetry()
		cmdutil.ErrorExit(log, err, errCommand)
	} else {
		endCommandSpan()
		log.Flush()
	}
}
