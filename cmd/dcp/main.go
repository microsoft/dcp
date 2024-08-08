package main

//go:generate goversioninfo

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcp/commands"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	errCommand = 1
	errSetup   = 2
	errPanic   = 3
)

func main() {
	log := logger.New("dcp")
	defer log.BeforeExit(func(value interface{}) {
		// Attempt to log the panic before exiting (we're already in a panic state, so the worst that can happen is that we panic again)
		log.Error(fmt.Errorf("panic: %v", value), "exiting due to panic")
		os.Exit(errPanic)
	})
	defer usvc_io.CleanupSessionFolderIfNeeded()

	ctx := kubeapiserver.SetupSignalContext()

	root, err := commands.NewRootCmd(log)
	if err != nil {
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	if err != nil {
		cmdutil.ErrorExit(log, err, errCommand)
	} else {
		log.Flush()
	}
}
