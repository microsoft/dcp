//go:build darwin && cgo

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package main

/*
#include <signal.h>

static int initial_handler_is_default;
static int initial_has_siginfo;
static int initial_flags;

__attribute__((constructor))
static void capture_initial_sigusr1(void) {
	struct sigaction action;
	if (sigaction(SIGUSR1, NULL, &action) == 0) {
		initial_handler_is_default = action.sa_handler == SIG_DFL;
		initial_has_siginfo = (action.sa_flags & SA_SIGINFO) != 0;
		initial_flags = action.sa_flags;
	}
}

static int get_initial_handler_is_default(void) {
	return initial_handler_is_default;
}

static int get_initial_has_siginfo(void) {
	return initial_has_siginfo;
}

static int get_initial_flags(void) {
	return initial_flags;
}
*/
import "C"

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/go-logr/logr"

	"github.com/microsoft/dcp/pkg/process"
)

const childArgument = "child"

func main() {
	if len(os.Args) == 2 && os.Args[1] == childArgument {
		validateChildSignalDisposition()
		return
	}

	launcherFlags := flag.NewFlagSet("signal-disposition", flag.ContinueOnError)
	launcherTimeout := launcherFlags.Duration("timeout", 30*time.Second, "Maximum time to wait for the child")
	if parseErr := launcherFlags.Parse(os.Args[1:]); parseErr != nil {
		os.Exit(2)
	}
	if launcherFlags.NArg() != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "unexpected arguments: %q\n", launcherFlags.Args())
		os.Exit(2)
	}

	runLauncher(*launcherTimeout)
}

func validateChildSignalDisposition() {
	handlerIsDefault := C.get_initial_handler_is_default() != 0
	hasSIGINFO := C.get_initial_has_siginfo() != 0
	flags := uint32(C.get_initial_flags())

	if !handlerIsDefault || hasSIGINFO {
		_, _ = fmt.Fprintf(
			os.Stderr,
			"SIGUSR1 at process startup: default=%t flags=%#x\n",
			handlerIsDefault,
			flags,
		)
		os.Exit(1)
	}
}

func runLauncher(timeout time.Duration) {
	executablePath, executablePathErr := os.Executable()
	if executablePathErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not determine signal disposition executable path: %v\n", executablePathErr)
		os.Exit(1)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), timeout)
	defer runCancel()

	childCmd := exec.Command(executablePath, childArgument)
	childCmd.Stdout = os.Stdout
	childCmd.Stderr = os.Stderr

	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()

	exitCode, runErr := process.RunToCompletion(runCtx, executor, childCmd)
	if runErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not run signal disposition child: %v\n", runErr)
		os.Exit(1)
	}
	if exitCode != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "signal disposition child exited with code %d\n", exitCode)
		os.Exit(1)
	}
}
