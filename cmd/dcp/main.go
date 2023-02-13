package main

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/usvc-dev/apiserver/internal/dcp/commands"
	"github.com/usvc-dev/apiserver/internal/logger"
)

func main() {
	logger, flushLogger := logger.NewLogger()
	ctrlruntime.SetLogger(logger)

	ctx := kubeapiserver.SetupSignalContext()

	root := commands.NewRootCmd()
	err := root.ExecuteContext(ctx)
	flushLogger()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
