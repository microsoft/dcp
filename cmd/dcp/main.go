package main

//go:generate goversioninfo

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	"github.com/microsoft/usvc-apiserver/internal/dcp/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	errCommand = 1
	errSetup   = 2
	errPanic   = 3
)

func main() {
	log := logger.New("dcp")
	defer log.BeforeExit(func(value interface{}) { os.Exit(errPanic) })
	defer logger.CleanupSessionFolderIfNeeded()

	ctx := kubeapiserver.SetupSignalContext()

	root, err := commands.NewRootCmd(log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errSetup)
	}

	err = root.ExecuteContext(ctx)

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommand)
	}
}
