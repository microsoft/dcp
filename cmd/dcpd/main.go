package main

//go:generate goversioninfo

import (
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcpd/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	errCommand = 1
	errSetup   = 2
	errPanic   = 3
)

func main() {
	log := logger.New("dcpd")
	defer log.BeforeExit(func(value interface{}) { os.Exit(errPanic) })

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
