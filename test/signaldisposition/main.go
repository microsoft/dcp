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
	"fmt"
	"os"
)

func main() {
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
