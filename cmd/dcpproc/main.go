package main

import (
	"context"
	"fmt"
	"os"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcpproc/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

//go:generate goversioninfo

const (
	errCommandError = 1
	errSetup        = 2
	errPanic        = 3
)

func main() {
	log := logger.New("dcpproc")
	defer log.BeforeExit(func(value interface{}) {
		// Attempt to log the panic before exiting (we're already in a panic state, so the worst that can happen is that we panic again)
		log.Error(fmt.Errorf("panic: %v", value), "exiting due to panic")
		os.Exit(errPanic)
	})

	ctx := context.Background()

	root, err := commands.NewRootCmd(log)
	if err != nil {
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	if err != nil {
		cmdutil.ErrorExit(log, err, errCommandError)
	} else {
		log.Flush()
	}
}
