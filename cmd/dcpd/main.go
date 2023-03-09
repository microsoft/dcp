package main

import (
	"context"
	"errors"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/usvc-dev/apiserver/internal/apiserver"
	"github.com/usvc-dev/apiserver/pkg/logger"
)

type DcpdExitCode int

const (
	OK              DcpdExitCode = 0
	ApiServerFailed DcpdExitCode = 1
)

func main() {
	logger, flushLogger := logger.NewLogger()
	ctrlruntime.SetLogger(logger)

	ctx, cancelFn := context.WithCancel(kubeapiserver.SetupSignalContext())

	apiServer := apiserver.NewApiServer("api-server", flushLogger)
	err := apiServer.Run(ctx)
	cancelFn()
	// The API server guarantees the logger is flushed before Run() exits
	if err == nil || errors.Is(err, context.Canceled) {
		os.Exit(int(OK))
	} else {
		os.Exit(int(ApiServerFailed))
	}
}
