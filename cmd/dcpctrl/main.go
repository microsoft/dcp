package main

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	"github.com/microsoft/usvc-apiserver/internal/dcpctrl/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	errCommandError = 1
	errPanic        = 3
)

func main() {
	log := logger.New("dcpctrl")
	defer log.BeforeExit(func(value interface{}) { os.Exit(errPanic) })

	ctx := kubeapiserver.SetupSignalContext()

	root := commands.NewRootCommand(log)
	err := root.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommandError)
	}
}
