// Copyright (c) Microsoft Corporation. All rights reserved.

package main

//go:generate goversioninfo

import (
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/dcpctrl/commands"
	"github.com/microsoft/dcp/internal/perftrace"
	"github.com/microsoft/dcp/internal/telemetry"
	"github.com/microsoft/dcp/pkg/logger"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/resiliency"
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

	ctx := kubeapiserver.SetupSignalContext()

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
