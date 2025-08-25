package main

//go:generate goversioninfo

import (
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcptun/commands"
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
	log := logger.New("dcptun").WithName("dcptun")
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

	root, err := commands.NewRootCommand(log)
	if err != nil {
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	_ = telemetrySystem.Shutdown(ctx)
	if err != nil {
		cmdutil.ErrorExit(log, err, errCommandError)
	} else {
		log.Flush()
	}
}
