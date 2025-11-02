// Copyright (c) Microsoft Corporation. All rights reserved.

package main

//go:generate goversioninfo

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcpctrl/commands"
	"github.com/microsoft/usvc-apiserver/internal/perftrace"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
)

const (
	errCommandError = 1
	errSetup        = 2
	errPanic        = 3
)

func main() {
	log := logger.New("dcpctrl").
		WithResourceSink().
		WithName("dcpctrl")

	defer func() {
		panicErr := resiliency.MakePanicError(recover(), log.Logger)
		logger.ReleaseAllResourceLogs()
		if panicErr != nil {
			os.Stderr.WriteString(panicErr.Error() + string(osutil.LineSep()))
			log.Flush()
			os.Exit(errPanic)
		}
	}()

	var ctx context.Context
	if runtime.GOOS == "windows" {
		ctx = context.Background()
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			for range c {
				// Suppressing signals to avoid children stopping dcp processes
			}
		}()
	} else {
		ctx = kubeapiserver.SetupSignalContext()
	}

	telemetrySystem := telemetry.GetTelemetrySystem()

	root, err := commands.NewRootCommand(log)
	if err != nil {
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	_ = telemetrySystem.Shutdown(ctx)
	perftrace.WaitProfilingComplete()
	if err != nil {
		cmdutil.ErrorExit(log, err, errCommandError)
	} else {
		log.Flush()
	}
}
