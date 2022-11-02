package main

import (
	"context"
	"flag"
	"os"

	serverbuilder "github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	serverstart "github.com/tilt-dev/tilt-apiserver/pkg/server/start"

	kubeapiserver "k8s.io/apiserver/pkg/server"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usvc-dev/apiserver/internal/ctrlmanager"
	"github.com/usvc-dev/apiserver/internal/logger"
)

type DcpdExitCode int

const (
	ServerOptionsErrorExit   DcpdExitCode = 1
	ServerExecutionErrorExit DcpdExitCode = 2
)

func main() {
	logger, flushLogger := logger.NewLogger()
	ctrlruntime.SetLogger(logger)
	log := runtimelog.Log.WithName("main")

	builder := serverbuilder.NewServerBuilder()
	options, err := builder.ToServerOptions()
	if err != nil {
		log.Error(err, "unable to create server options")
		flushLogger()
		os.Exit(int(ServerOptionsErrorExit))
	}

	// TODO
	// The API server and controller manager functionality should be run
	// via hosting package

	ctx, cancelFn := context.WithCancel(kubeapiserver.SetupSignalContext())

	// Start controller manager
	managerStopCh := make(chan struct{})
	go func() {
		// The controller manager will do all the logging so there is no need to handle the error from running it
		_ = ctrlmanager.RunManager(options.ServingOptions.BindPort, flushLogger, ctx)
		close(managerStopCh)
	}()

	// Run the API server
	cmd := serverstart.NewCommandStartTiltServer(options, ctx)
	cmd.Flags().AddGoFlagSet(flag.CommandLine)
	err = cmd.Execute()
	if err != nil {
		log.Error(err, "server execution error")
		flushLogger()
		os.Exit(int(ServerExecutionErrorExit))
	}

	cancelFn()
}
