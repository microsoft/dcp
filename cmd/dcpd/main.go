package main

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	"github.com/usvc-dev/apiserver/internal/dcpd/commands"
)

const (
	errCommand = 1
	errSetup   = 2
)

func main() {
	ctx := kubeapiserver.SetupSignalContext()

	root, err := commands.NewRootCmd()
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
