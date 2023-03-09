package main

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/usvc-dev/apiserver/internal/dcp/commands"
	"github.com/usvc-dev/apiserver/pkg/logger"
)

const (
	errCommand = 1
	errSetup   = 2
)

func main() {
	logger, flushLogger := logger.NewLogger()
	ctrlruntime.SetLogger(logger)

	ctx := kubeapiserver.SetupSignalContext()

	root, err := commands.NewRootCmd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errSetup)
	}

	err = root.ExecuteContext(ctx)
	flushLogger()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommand)
	}
}
