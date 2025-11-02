// Copyright (c) Microsoft Corporation. All rights reserved.

package main

//go:generate goversioninfo

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcp/commands"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
)

const (
	errCommand = 1
	errSetup   = 2
	errPanic   = 3
)

func main() {
	log := logger.New("dcp").
		WithFilterSink(logger.MacOsProcErrorLogFilter, 1).
		WithName("dcp")
	defer func() {
		panicErr := resiliency.MakePanicError(recover(), log.Logger)
		if panicErr != nil {
			os.Stderr.WriteString(panicErr.Error() + string(osutil.LineSep()))
			log.Flush()
			os.Exit(errPanic)
		}
	}()
	defer usvc_io.CleanupSessionFolderIfNeeded()

	var ctx context.Context
	if runtime.GOOS == "windows" {
		ctx = context.Background()
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			for range c {
				// Suppressing signals to avoid children stopping dcp processes
			}
		}()
	} else {
		ctx = kubeapiserver.SetupSignalContext()
	}

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
