package main

import (
	"fmt"
	"os"

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

	root, err := commands.NewRootCmd(log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errSetup)
	}

	err = root.Execute()

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommand)
	}
}
