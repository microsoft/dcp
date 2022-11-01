package main

import (
	"flag"
	"os"

	serverbuilder "github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	serverstart "github.com/tilt-dev/tilt-apiserver/pkg/server/start"
	"github.com/usvc-dev/apiserver/internal/logger"
	kubeapiserver "k8s.io/apiserver/pkg/server"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"
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

	ctx := kubeapiserver.SetupSignalContext()
	cmd := serverstart.NewCommandStartTiltServer(options, ctx)
	cmd.Flags().AddGoFlagSet(flag.CommandLine)

	err = cmd.Execute()
	if err != nil {
		log.Error(err, "server execution error")
		flushLogger()
		os.Exit(int(ServerExecutionErrorExit))
	}
}
