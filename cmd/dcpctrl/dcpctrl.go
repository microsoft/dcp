package main

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	"github.com/microsoft/usvc-apiserver/internal/commands/dcpctrl"
)

const (
	errCommandError = 1
)

func main() {
	ctx := kubeapiserver.SetupSignalContext()
	root := dcpctrl.NewRootCommand()
	err := root.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommandError)
	}
}
