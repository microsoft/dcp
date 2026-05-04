/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

//go:generate goversioninfo

import (
	"context"
	"os"
	"path/filepath"
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

	ctx, commandSpan := telemetry.StartStartupSpan(ctx, telemetry.StartupSpanCommand)
	telemetry.SetAttribute(ctx, telemetry.StartupAttributeProcessPID, os.Getpid())
	telemetry.SetAttribute(ctx, telemetry.StartupAttributeProcessExecutableName, filepath.Base(os.Args[0]))
	if len(os.Args) > 1 {
		telemetry.SetAttribute(ctx, telemetry.StartupAttributeCommandArgumentCount, len(os.Args)-1)
	}
	if commandName := getCommandName(os.Args); commandName != "" {
		telemetry.SetAttribute(ctx, telemetry.StartupAttributeCommandName, commandName)
	}
	spanEnded := false
	endCommandSpan := func() {
		if !spanEnded {
			commandSpan.End()
			spanEnded = true
		}
	}
	defer endCommandSpan()

	telemetry.AddEvent(ctx, telemetry.StartupEventCommandRootCommandCreating)
	root, err := commands.NewRootCmd(log)
	if err != nil {
		telemetry.SetAttribute(ctx, telemetry.StartupAttributeCommandExitCode, errSetup)
		commandSpan.SetError(err)
		endCommandSpan()
		shutdownTelemetry()
		cmdutil.ErrorExit(log, err, errSetup)
	}
	telemetry.AddEvent(ctx, telemetry.StartupEventCommandRootCommandCreated)

	telemetry.AddEvent(ctx, telemetry.StartupEventCommandExecuteStart)
	endCommandSpan()

	lifetimeCtx, lifetimeSpan := telemetry.StartStartupSpan(ctx, telemetry.StartupSpanCommandLifetime)
	if commandName := getCommandName(os.Args); commandName != "" {
		telemetry.SetAttribute(lifetimeCtx, telemetry.StartupAttributeCommandName, commandName)
	}
	telemetry.SetAttribute(lifetimeCtx, telemetry.StartupAttributeCommandArgumentCount, max(len(os.Args)-1, 0))

	err = root.ExecuteContext(lifetimeCtx)
	if err != nil {
		telemetry.SetAttribute(lifetimeCtx, telemetry.StartupAttributeCommandExitCode, errCommand)
		telemetry.AddEvent(lifetimeCtx, telemetry.StartupEventCommandExecuteFailed)
		lifetimeSpan.SetError(err)
		lifetimeSpan.End()
		shutdownTelemetry()
		cmdutil.ErrorExit(log, err, errCommand)
	} else {
		telemetry.SetAttribute(lifetimeCtx, telemetry.StartupAttributeCommandExitCode, 0)
		telemetry.AddEvent(lifetimeCtx, telemetry.StartupEventCommandExecuteSucceeded)
		lifetimeSpan.End()
		log.Flush()
	}
}

func getCommandName(args []string) string {
	for _, arg := range args[1:] {
		if arg != "" && arg[0] != '-' {
			return arg
		}
	}

	return ""
}
